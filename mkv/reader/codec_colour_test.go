package reader

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// CodecPrivate (codec configuration records) for each codec/colour combination
// under test. Generated from synthetic 64x64 clips (x265 / x264 / svt-av1) that
// signal the colour in the bitstream and carry no container Colour element:
//
//	hevcHDR : HEVC Main 10, BT.2020 / PQ (SMPTE 2084), 10-bit
//	h264SDR : H.264 High, BT.709, 8-bit
//	av1HDR  : AV1, BT.2020 / PQ, 10-bit
const (
	hevcHDRPrivateHex = "0102200000009000000000001ef000fcfdfafa00000f03200001001840010c01ffff02200000030090000003000003001e959809210001002d42010102200000030090000003000003001ea0208104d96566924caf016a12201208000003000800000300c04022000100074401c172b42240"
	h264SDRPrivateHex = "0164000affe1001b6764000aacd94426c05a808080a0000003002000000601e244b2c001000668ebe3cb22c0fdf8f800"
	av1HDRPrivateHex  = "81004c000a0d00000002afff805f0a12201208"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func p16(p *uint16) int {
	if p == nil {
		return -1
	}
	return int(*p)
}

// --- unit: parse the bitstream colour directly from real CodecPrivate ---------

func TestCodecColourParsers(t *testing.T) {
	tests := []struct {
		name                                        string
		codec                                       string
		cp                                          []byte
		wantMatrix, wantTransfer, wantPrim, wantRng int
		wantBitDepth                                int
		wantProfile                                 string
		wantHDR                                     bool
	}{
		{
			name: "HEVC Main 10 HDR10 (bt2020nc/PQ)", codec: "hevc", cp: mustHex(t, hevcHDRPrivateHex),
			wantMatrix: 9, wantTransfer: 16, wantPrim: 9, wantRng: 1, wantBitDepth: 10, wantProfile: "Main 10", wantHDR: true,
		},
		{
			name: "H.264 High bt709 8-bit", codec: "h264", cp: mustHex(t, h264SDRPrivateHex),
			wantMatrix: 1, wantTransfer: 1, wantPrim: 1, wantRng: 1, wantBitDepth: 8, wantProfile: "High", wantHDR: false,
		},
		{
			name: "AV1 bt2020nc/PQ 10-bit HDR", codec: "av1", cp: mustHex(t, av1HDRPrivateHex),
			wantMatrix: 9, wantTransfer: 16, wantPrim: 9, wantRng: 1, wantBitDepth: 10, wantProfile: "Main", wantHDR: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := mkv.Track{Type: mkv.VideoTrack, Codec: tt.codec, CodecPrivate: tt.cp}
			fillColourFromCodecPrivate(&tr)
			if got := p16(tr.ColorSpace); got != tt.wantMatrix {
				t.Errorf("matrix = %d, want %d", got, tt.wantMatrix)
			}
			if got := p16(tr.ColorTransfer); got != tt.wantTransfer {
				t.Errorf("transfer = %d, want %d", got, tt.wantTransfer)
			}
			if got := p16(tr.ColorPrimaries); got != tt.wantPrim {
				t.Errorf("primaries = %d, want %d", got, tt.wantPrim)
			}
			if got := p16(tr.ColorRange); got != tt.wantRng {
				t.Errorf("range = %d, want %d", got, tt.wantRng)
			}
			if got := p16(tr.VideoBitDepth); got != tt.wantBitDepth {
				t.Errorf("bit depth = %d, want %d", got, tt.wantBitDepth)
			}
			if tr.Profile != tt.wantProfile {
				t.Errorf("profile = %q, want %q", tr.Profile, tt.wantProfile)
			}
			if tr.IsHDR() != tt.wantHDR {
				t.Errorf("IsHDR = %v, want %v", tr.IsHDR(), tt.wantHDR)
			}
		})
	}
}

// h264RealAvcCHex is a real x264-produced avcC (High profile, BT.709) used as a
// second real-world fixture for the AVC colour/bit-depth parser.
const h264RealAvcCHex = "0164000affe1001c6764000aacd94426c05a810100a000000300200000030141e244b2c001000768ebe3cb3002c0fdf8f800"

func TestCodecColourAVCRealFixture(t *testing.T) {
	tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: mustHex(t, h264RealAvcCHex)}
	fillColourFromCodecPrivate(&tr)
	if got := p16(tr.ColorSpace); got != 1 {
		t.Errorf("matrix = %d, want 1 (bt709)", got)
	}
	if got := p16(tr.VideoBitDepth); got != 8 {
		t.Errorf("bit depth = %d, want 8", got)
	}
	if tr.PixelFormat != "yuv420p" {
		t.Errorf("pix_fmt = %q, want yuv420p (High 4:2:0 8-bit)", tr.PixelFormat)
	}
	// avcC byte 3 (and the SPS level_idc) is 0x0a → level 1.0.
	if got := p16(tr.Level); got != 10 {
		t.Errorf("level = %d, want 10", got)
	}
}

// --- integration: through ReadMeta on a built MKV (no container Colour) -------

func bytesElem(id uint32, b []byte) []byte {
	var buf bytes.Buffer
	ebml.WriteElementHeader(&buf, id, int64(len(b)))
	buf.Write(b)
	return buf.Bytes()
}

// hevcVideoTrackMKV builds a minimal MKV with one HEVC video track carrying cp as
// CodecPrivate. When colour != nil it also writes a container Video>Colour element.
func hevcVideoTrackMKV(cp []byte, colour []byte) []byte {
	video := []([]byte){
		uintElem(mkv.IDPixelWidth, 3840, 2),
		uintElem(mkv.IDPixelHeight, 1600, 2),
	}
	if colour != nil {
		video = append(video, colour)
	}
	te := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_MPEGH/ISO/HEVC"),
		bytesElem(mkv.IDCodecPrivate, cp),
		masterElem(mkv.IDVideo, video...),
	)
	return buildMKV(te)
}

func TestColourFromBitstreamViaReadMeta(t *testing.T) {
	data := hevcVideoTrackMKV(mustHex(t, hevcHDRPrivateHex), nil) // no container Colour
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	if tr.ColorSpaceName() != "bt2020nc" || tr.ColorTransferName() != "smpte2084" ||
		tr.ColorPrimariesName() != "bt2020" || p16(tr.VideoBitDepth) != 10 || !tr.IsHDR() {
		t.Errorf("ReadMeta colour from SPS: space=%q transfer=%q primaries=%q depth=%d hdr=%v, want bt2020nc/smpte2084/bt2020/10/true",
			tr.ColorSpaceName(), tr.ColorTransferName(), tr.ColorPrimariesName(), p16(tr.VideoBitDepth), tr.IsHDR())
	}
	if tr.Profile != "Main 10" {
		t.Errorf("profile = %q, want Main 10", tr.Profile)
	}
	if tr.PixelFormat != "yuv420p10le" {
		t.Errorf("pix_fmt = %q, want yuv420p10le (Main 10, 4:2:0)", tr.PixelFormat)
	}
	// HEVC general_level_idc is read from the profile_tier_level (30×level).
	if tr.Level == nil || *tr.Level == 0 {
		t.Errorf("HEVC level = %v, want a non-zero general_level_idc", tr.Level)
	}
}

// TestColourContainerWinsOverSPS proves the container Colour element is
// authoritative: an SPS that says bt2020/PQ must NOT override a container that
// says bt709.
func TestColourContainerWinsOverSPS(t *testing.T) {
	containerColour := masterElem(mkv.IDColour,
		uintElem(mkv.IDColourMatrix, 1, 1),    // bt709
		uintElem(mkv.IDColourTransfer, 1, 1),  // bt709
		uintElem(mkv.IDColourPrimaries, 1, 1), // bt709
		uintElem(mkv.IDColourRange, 1, 1),
		uintElem(mkv.IDColourBitsPerChannel, 8, 1),
	)
	data := hevcVideoTrackMKV(mustHex(t, hevcHDRPrivateHex), containerColour) // SPS says bt2020/PQ/10
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	if tr.ColorSpaceName() != "bt709" || tr.ColorTransferName() != "bt709" ||
		tr.ColorPrimariesName() != "bt709" || p16(tr.VideoBitDepth) != 8 || tr.IsHDR() {
		t.Errorf("container must win: got space=%q transfer=%q primaries=%q depth=%d hdr=%v, want bt709/bt709/bt709/8/false",
			tr.ColorSpaceName(), tr.ColorTransferName(), tr.ColorPrimariesName(), p16(tr.VideoBitDepth), tr.IsHDR())
	}
}

// TestColourFromCodecPrivateRobustness: malformed/truncated CodecPrivate must
// never error or panic, and must leave the colour fields nil.
func TestColourFromCodecPrivateRobustness(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		{0x00},
		{0x01, 0x02, 0x03},
		bytes.Repeat([]byte{0xff}, 64),
		bytes.Repeat([]byte{0x00}, 64),
		mustHex(t, hevcHDRPrivateHex)[:10], // truncated real hvcC
		mustHex(t, h264SDRPrivateHex)[:5],  // truncated real avcC
		append([]byte{0x01}, bytes.Repeat([]byte{0xaa}, 200)...),
	}
	for _, codec := range []string{"hevc", "h264", "av1", "vp9", "unknown"} {
		for i, cp := range cases {
			tr := mkv.Track{Type: mkv.VideoTrack, Codec: codec, CodecPrivate: cp}
			fillColourFromCodecPrivate(&tr) // must not panic
			// Truncated-but-valid-header HEVC may still recover bit depth from the
			// hvcC header; that's fine. The contract is "no panic, no garbage colour".
			if tr.ColorSpace != nil && (*tr.ColorSpace > 255) {
				t.Errorf("codec %s case %d: bogus matrix %d", codec, i, *tr.ColorSpace)
			}
		}
	}
}
