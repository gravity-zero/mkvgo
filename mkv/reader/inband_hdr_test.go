package reader

import (
	"bytes"
	"context"
	"math"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// hdrSEINAL builds a prefix SEI NAL carrying a mastering_display_colour_volume
// (137) and a content_light_level_info (144) message. The mastering payload is
// the BT.2020 / D65 / 1000-0.0001 nit volume real HDR10 masters declare, in the
// SEI's G, B, R primary order; the luminance fields are chosen so the RBSP holds
// a 00 00 03 run, which checks the emulation-prevention unescape on this path.
func hdrSEINAL() []byte {
	var p []byte
	u16 := func(v uint16) { p = append(p, byte(v>>8), byte(v)) }
	u32 := func(v uint32) { p = append(p, byte(v>>24), byte(v>>16), byte(v>>8), byte(v)) }
	// --- 137: display_primaries (G, B, R), white point, max/min luminance
	p = append(p, 137, 24)
	u16(13250)
	u16(34500) // G
	u16(7500)
	u16(3000) // B
	u16(34000)
	u16(16000) // R
	u16(15635)
	u16(16450)      // D65
	u32(10_000_000) // 1000 cd/m² in 0.0001 units: 00 98 96 80
	u32(3)          // 0.0003 cd/m²: 00 00 00 03 → escaped as 00 00 03 00 03 on the wire
	// --- 144: MaxCLL 957, MaxFALL 143
	p = append(p, 144, 4)
	u16(957)
	u16(143)
	p = append(p, 0x80) // rbsp_trailing_bits
	// Emulation prevention: any 00 00 followed by 00..03 gets a 03 inserted.
	var esc []byte
	zeros := 0
	for _, b := range p {
		if zeros >= 2 && b <= 3 {
			esc = append(esc, 3)
			zeros = 0
		}
		esc = append(esc, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return append([]byte{0x4e, 0x01}, esc...) // nal_unit_type 39, layer 0, tid 1
}

// hdrTrackMKV builds an MKV with one video track (codec, CodecPrivate, Colour
// code points, optional Colour HDR children) and one cluster whose first
// SimpleBlock holds frame.
func hdrTrackMKV(codecID string, cp []byte, transfer uint64, colourHDR []byte, frame []byte) []byte {
	colour := []byte{}
	colour = append(colour, uintElem(mkv.IDColourMatrix, 9, 1)...)
	colour = append(colour, uintElem(mkv.IDColourTransfer, transfer, 1)...)
	colour = append(colour, uintElem(mkv.IDColourPrimaries, 9, 1)...)
	colour = append(colour, colourHDR...)
	te := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, codecID),
		bytesElem(mkv.IDCodecPrivate, cp),
		masterElem(mkv.IDVideo,
			uintElem(mkv.IDPixelWidth, 3840, 2),
			uintElem(mkv.IDPixelHeight, 2160, 2),
			masterElem(mkv.IDColour, colour),
		),
	)
	info := masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 4))
	tracks := masterElem(mkv.IDTracks, te)
	block := append([]byte{0x81, 0x00, 0x00, 0x80}, frame...) // track 1, relTC 0, keyframe
	cluster := clusterWithSimpleBlock(block)

	var seg bytes.Buffer
	seg.Write(info)
	seg.Write(tracks)
	ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(len(cluster)))
	seg.Write(cluster)
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	return buf.Bytes()
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// wantBT2020Mastering checks the mastering display the fixtures declare,
// in MasteringDisplay units (chromaticity 0..1, cd/m²).
func wantBT2020Mastering(t *testing.T, md *mkv.MasteringDisplay, lumMin float64) {
	t.Helper()
	if md == nil {
		t.Fatal("MasteringDisplay = nil, want the in-band volume")
	}
	if !near(md.RedX, 0.68) || !near(md.RedY, 0.32) || !near(md.GreenX, 0.265) || !near(md.GreenY, 0.69) ||
		!near(md.BlueX, 0.15) || !near(md.BlueY, 0.06) || !near(md.WhiteX, 0.3127) || !near(md.WhiteY, 0.329) {
		t.Errorf("primaries = %+v, want BT.2020 / D65 (R .68,.32 G .265,.69 B .15,.06 W .3127,.329)", *md)
	}
	if !near(md.LuminanceMax, 1000) || !near(md.LuminanceMin, lumMin) {
		t.Errorf("luminance = %g..%g, want %g..1000", md.LuminanceMin, md.LuminanceMax, lumMin)
	}
}

// TestInBandHDRStaticFromHEVCSEI proves the opt-in completes an HDR10 track
// whose container carries PQ colour but no static metadata, from the SEI of the
// first sample - and that the default head-only read still reports none.
func TestInBandHDRStaticFromHEVCSEI(t *testing.T) {
	hvcC := mustHex(t, hevcHDRPrivateHex) // full record, SPS in-record: no colour fallback needed
	frame := lenPrefixed4(hdrSEINAL())
	data := hdrTrackMKV("V_MPEGH/ISO/HEVC", hvcC, 16, nil, frame)

	base, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if base.Tracks[0].HDR != nil {
		t.Fatalf("without the option HDR must stay nil, got %+v", base.Tracks[0].HDR)
	}

	for name, open := range map[string]func() (*mkv.Container, error){
		"ReadMeta": func() (*mkv.Container, error) {
			return ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv", WithInBandColourFallback())
		},
		"Read": func() (*mkv.Container, error) {
			return Read(context.Background(), bytes.NewReader(data), "x.mkv", WithInBandColourFallback())
		},
	} {
		c, err := open()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		h := c.Tracks[0].HDR
		if h == nil {
			t.Fatalf("%s: HDR = nil, want the SEI static metadata", name)
		}
		if h.MaxCLL != 957 || h.MaxFALL != 143 {
			t.Errorf("%s: CLL = %d/%d, want 957/143", name, h.MaxCLL, h.MaxFALL)
		}
		wantBT2020Mastering(t, h.MasteringDisplay, 0.0003)
	}
}

// TestInBandHDRContainerWinsPerPart proves the container's part is never
// overwritten: a Colour element with MaxCLL/MaxFALL but no MasteringMetadata
// keeps its light level and gains only the mastering display from the SEI.
func TestInBandHDRContainerWinsPerPart(t *testing.T) {
	hvcC := mustHex(t, hevcHDRPrivateHex)
	cll := append(uintElem(mkv.IDColourMaxCLL, 600, 2), uintElem(mkv.IDColourMaxFALL, 200, 1)...)
	data := hdrTrackMKV("V_MPEGH/ISO/HEVC", hvcC, 16, cll, lenPrefixed4(hdrSEINAL()))

	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv", WithInBandColourFallback())
	if err != nil {
		t.Fatal(err)
	}
	h := c.Tracks[0].HDR
	if h == nil || h.MaxCLL != 600 || h.MaxFALL != 200 {
		t.Fatalf("CLL = %+v, want the container's 600/200 kept over the SEI's 957/143", h)
	}
	wantBT2020Mastering(t, h.MasteringDisplay, 0.0003)
}

// TestInBandHDRSkipsSDR proves an SDR track (BT.709 transfer) never triggers the
// read: the same SEI in its first sample leaves HDR nil.
func TestInBandHDRSkipsSDR(t *testing.T) {
	hvcC := mustHex(t, hevcHDRPrivateHex)
	data := hdrTrackMKV("V_MPEGH/ISO/HEVC", hvcC, 1, nil, lenPrefixed4(hdrSEINAL()))
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv", WithInBandColourFallback())
	if err != nil {
		t.Fatal(err)
	}
	if c.Tracks[0].HDR != nil {
		t.Errorf("SDR track: HDR = %+v, want nil (no in-band read for a non-HDR transfer)", c.Tracks[0].HDR)
	}
}

// av1MetadataTU builds an AV1 temporal unit of sized OBUs: a temporal
// delimiter, an HDR_CLL and an HDR_MDCV metadata OBU, then a stub frame OBU.
func av1MetadataTU() []byte {
	obu := func(typ byte, payload []byte) []byte {
		return append([]byte{typ<<3 | 0x02, byte(len(payload))}, payload...) // has_size_field, size < 128
	}
	var mdcv []byte
	u16 := func(v uint16) { mdcv = append(mdcv, byte(v>>8), byte(v)) }
	u32 := func(v uint32) { mdcv = append(mdcv, byte(v>>24), byte(v>>16), byte(v>>8), byte(v)) }
	fx := func(c float64) uint16 { return uint16(math.Round(c * 65536)) }
	u16(fx(0.68))
	u16(fx(0.32)) // R
	u16(fx(0.265))
	u16(fx(0.69)) // G
	u16(fx(0.15))
	u16(fx(0.06)) // B
	u16(fx(0.3127))
	u16(fx(0.329)) // W
	u32(1000 << 8) // luminance_max 24.8
	u32(82)        // luminance_min 18.14: 82/16384 = 0.0050048 cd/m² (the closest step to 0.005)
	var tu []byte
	tu = append(tu, obu(2, nil)...)                               // OBU_TEMPORAL_DELIMITER
	tu = append(tu, obu(5, []byte{1, 0x03, 0xbd, 0x00, 0x8f})...) // HDR_CLL: 957 / 143
	tu = append(tu, obu(5, append([]byte{2}, mdcv...))...)        // HDR_MDCV
	tu = append(tu, obu(6, []byte{0xde, 0xad, 0xbe, 0xef})...)    // OBU_FRAME stub
	return tu
}

// av1C is a minimal AV1CodecConfigurationRecord (marker|version 1, Main profile,
// seq_level_idx 12, 10-bit, no configOBUs): the colour then comes from the
// container, as mainstream AV1 muxes write it.
func av1C() []byte { return []byte{0x81, 0x0c, 0x4c, 0x00} }

// TestInBandHDRStaticFromAV1Metadata proves the AV1 side: HDR_CLL and HDR_MDCV
// metadata OBUs of the first temporal unit complete a PQ track the container
// left without static metadata, with the AV1 fixed-point units converted.
func TestInBandHDRStaticFromAV1Metadata(t *testing.T) {
	data := hdrTrackMKV("V_AV1", av1C(), 16, nil, av1MetadataTU())
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv", WithInBandColourFallback())
	if err != nil {
		t.Fatal(err)
	}
	h := c.Tracks[0].HDR
	if h == nil {
		t.Fatal("HDR = nil, want the metadata OBUs' static metadata")
	}
	if h.MaxCLL != 957 || h.MaxFALL != 143 {
		t.Errorf("CLL = %d/%d, want 957/143", h.MaxCLL, h.MaxFALL)
	}
	md := h.MasteringDisplay
	if md == nil {
		t.Fatal("MasteringDisplay = nil")
	}
	// 0.16 fixed point rounds each chromaticity to 1/65536.
	for name, got := range map[string][2]float64{
		"R": {md.RedX, md.RedY}, "G": {md.GreenX, md.GreenY}, "B": {md.BlueX, md.BlueY}, "W": {md.WhiteX, md.WhiteY},
	} {
		want := map[string][2]float64{"R": {0.68, 0.32}, "G": {0.265, 0.69}, "B": {0.15, 0.06}, "W": {0.3127, 0.329}}[name]
		if math.Abs(got[0]-want[0]) > 1.0/65536 || math.Abs(got[1]-want[1]) > 1.0/65536 {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if !near(md.LuminanceMax, 1000) || math.Abs(md.LuminanceMin-0.005) > 1.0/16384 {
		t.Errorf("luminance = %g..%g, want 0.005..1000", md.LuminanceMin, md.LuminanceMax)
	}
}

// TestInBandHDRNeedsOnlyIncomplete pins the selection rule: a PQ track whose
// container already carries both parts is not a candidate (no sample read), an
// HLG one without them is, and a non-HEVC/AV1 codec never is.
func TestInBandHDRNeedsOnlyIncomplete(t *testing.T) {
	pq, hlg := uint16(16), uint16(18)
	full := &mkv.HDRStaticMetadata{MaxCLL: 1, MaxFALL: 1, MasteringDisplay: &mkv.MasteringDisplay{
		RedX: .68, RedY: .32, GreenX: .265, GreenY: .69, BlueX: .15, BlueY: .06, WhiteX: .3127, WhiteY: .329, LuminanceMax: 1000}}
	cases := []struct {
		name string
		t    mkv.Track
		want bool
	}{
		{"hevc pq, nothing", mkv.Track{Type: mkv.VideoTrack, Codec: "hevc", ColorTransfer: &pq}, true},
		{"hevc pq, complete", mkv.Track{Type: mkv.VideoTrack, Codec: "hevc", ColorTransfer: &pq, HDR: full}, false},
		{"av1 hlg, nothing", mkv.Track{Type: mkv.VideoTrack, Codec: "av1", ColorTransfer: &hlg}, true},
		{"vp9 pq", mkv.Track{Type: mkv.VideoTrack, Codec: "vp9", ColorTransfer: &pq}, false},
		{"hevc, transfer unknown", mkv.Track{Type: mkv.VideoTrack, Codec: "hevc"}, false},
		{"audio", mkv.Track{Type: mkv.AudioTrack, Codec: "hevc", ColorTransfer: &pq}, false},
	}
	for _, c := range cases {
		if got := needsInBandHDR(&c.t); got != c.want {
			t.Errorf("%s: needsInBandHDR = %v, want %v", c.name, got, c.want)
		}
	}
}
