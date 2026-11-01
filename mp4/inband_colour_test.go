package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// A real HEVC HDR hvcC (Main 10, bt2020nc / PQ / 10-bit) WITH its SPS in the NAL
// arrays - the SPS is lifted out of it and replanted in-band for these tests.
const hevcHDRPrivateHex = "0102200000009000000000001ef000fcfdfafa00000f03200001001840010c01ffff02200000030090000003000003001e959809210001002d42010102200000030090000003000003001ea0208104d96566924caf016a12201208000003000800000300c04022000100074401c172b42240"

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	return b
}

// bareHvcC is a 23-byte HEVCDecoderConfigurationRecord with numOfArrays==0 (no
// SPS), as streaming-style muxes write when the parameter sets live in-band.
func bareHvcC() []byte {
	cp := make([]byte, 23)
	cp[0] = 1     // version
	cp[1] = 2     // Main 10
	cp[17] = 2    // bitDepthLumaMinus8 → 10-bit
	cp[18] = 2    // bitDepthChromaMinus8
	cp[21] = 0xff // lengthSizeMinusOne (low 2 bits) = 3 → 4-byte NAL prefix
	cp[22] = 0    // numOfArrays = 0
	return cp
}

func extractHEVCSPSNAL(t *testing.T, hvcC []byte) []byte {
	t.Helper()
	off := 23
	for a := 0; a < int(hvcC[22]); a++ {
		nalType := hvcC[off] & 0x3f
		count := int(hvcC[off+1])<<8 | int(hvcC[off+2])
		off += 3
		for n := 0; n < count; n++ {
			ln := int(hvcC[off])<<8 | int(hvcC[off+1])
			off += 2
			nal := hvcC[off : off+ln]
			off += ln
			if nalType == 33 {
				return nal
			}
		}
	}
	t.Fatal("no SPS NAL in hvcC")
	return nil
}

func lenPrefixed4(nal []byte) []byte {
	n := len(nal)
	return append([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}, nal...)
}

// atcSEINAL builds an Alternative Transfer Characteristics SEI NAL (prefix SEI,
// payload type 147) carrying preferred_transfer_characteristics = transfer.
func atcSEINAL(transfer byte) []byte {
	return []byte{0x4e, 0x01, 0x93, 0x01, transfer, 0x80}
}

// TestMP4InBandColourFallback proves the MP4 counterpart of the MKV in-band
// fallback: an HEVC track with a bare hvcC and no colr box keeps its colour
// in-band; the opt-in reads the first sample and recovers the SPS VUI plus the
// ATC SEI override (here arib-std-b67), while the default leaves colour nil.
func TestMP4InBandColourFallback(t *testing.T) {
	sps := extractHEVCSPSNAL(t, mustHex(t, hevcHDRPrivateHex)) // SPS VUI transfer = PQ
	frame := append(lenPrefixed4(sps), lenPrefixed4(atcSEINAL(18))...)

	w, h := uint32(3840), uint32(2160)
	tracks := []mkv.Track{{
		ID: 1, Type: mkv.VideoTrack, Codec: "V_MPEGH/ISO/HEVC",
		CodecPrivate: bareHvcC(), Width: &w, Height: &h,
	}}
	mkvPath := buildMKV(t, tracks, []genBlock{{track: 1, pts: 0, key: true, data: frame}})
	mp4Bytes, _ := remux(t, mkvPath)

	// The remux itself applies the in-band fallback, so the MP4 it writes carries
	// a colr box with the recovered colour - the ATC SEI's transfer included.
	written, _, err := ReadMeta(context.Background(), bytes.NewReader(mp4Bytes), "x.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if len(written.Tracks) == 0 {
		t.Fatal("video track was dropped by the remux")
	}
	if tr := written.Tracks[0]; tr.ColorTransferName() != "arib-std-b67" || tr.ColorSpaceName() != "bt2020nc" {
		t.Fatalf("remux must write the in-band colour as colr, got transfer=%q space=%q", tr.ColorTransferName(), tr.ColorSpaceName())
	}
	// Neutralise that colr (same-size rename to a free box) to get the MP4 a
	// muxer that kept the colour in-band writes: bare hvcC, no colr.
	if n := bytes.Count(mp4Bytes, []byte("colr")); n != 1 {
		t.Fatalf("expected exactly one colr box in the fixture, found %d", n)
	}
	mp4Bytes = bytes.Replace(mp4Bytes, []byte("colr"), []byte("free"), 1)

	// Default: head-only, bare hvcC + no colr → no colour.
	base, _, err := ReadMeta(context.Background(), bytes.NewReader(mp4Bytes), "x.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Tracks) == 0 {
		t.Fatal("video track was dropped by the remux")
	}
	if tr := base.Tracks[0]; tr.ColorTransfer != nil || tr.ColorSpace != nil {
		t.Fatalf("without the option colour must stay nil, got transfer=%v space=%v", tr.ColorTransfer, tr.ColorSpace)
	}

	// Opted-in: the first sample's SPS + ATC SEI are read.
	c, _, err := ReadMeta(context.Background(), bytes.NewReader(mp4Bytes), "x.mp4", Options{InBandColour: true})
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	if tr.ColorSpaceName() != "bt2020nc" || tr.ColorTransferName() != "arib-std-b67" ||
		tr.ColorPrimariesName() != "bt2020" || !tr.IsHDR() {
		t.Errorf("MP4 in-band colour: space=%q transfer=%q primaries=%q hdr=%v, want bt2020nc/arib-std-b67/bt2020/true",
			tr.ColorSpaceName(), tr.ColorTransferName(), tr.ColorPrimariesName(), tr.IsHDR())
	}
}

// TestMP4InBandScanX264: x264 MBAFF and fake-interlaced files ffmpeg wrote. With
// the in-band option the first samples settle the scan type: a tff stream is
// interlaced/tt, a stream whose 16th sample turns bottom-first keeps no order
// (one sample read would have said tt), and fake-interlaced - field coding
// allowed, frames throughout - stays unknown. Without the option all stay "".
func TestMP4InBandScanX264(t *testing.T) {
	for file, want := range map[string][2]string{
		"h264_mbaff_tff.mp4":         {"interlaced", "tt"},
		"h264_mbaff_mixed_order.mp4": {"interlaced", ""},
		"h264_fake_interlaced.mp4":   {"", ""},
	} {
		data, err := os.ReadFile("../internal/testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
		for _, opt := range []bool{false, true} {
			c, _, err := ReadMeta(context.Background(), bytes.NewReader(data), file, Options{InBandColour: opt})
			if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			got, exp := c.Tracks[0], want
			if !opt {
				exp = [2]string{"", ""}
			}
			if got.ScanType != exp[0] || got.FieldOrder != exp[1] {
				t.Errorf("%s (in-band %v): ScanType/FieldOrder = %q/%q, want %q/%q", file, opt, got.ScanType, got.FieldOrder, exp[0], exp[1])
			}
		}
	}
}

// TestHeadSampleLocs walks stsz/stsc/stco across chunks and stops at the zeroed
// remainder a head-only read leaves.
func TestHeadSampleLocs(t *testing.T) {
	u32 := func(vs ...uint32) []byte {
		var b []byte
		for _, v := range vs {
			b = binary.BigEndian.AppendUint32(b, v)
		}
		return b
	}
	boxes := []memBox{
		{typ: "stsz", payload: u32(0, 0, 5, 10, 20, 30, 40, 0)}, // 5 samples, the 5th zeroed
		{typ: "stsc", payload: u32(0, 2, 1, 2, 1, 2, 1, 1)},     // chunk 1: 2 samples, then 1 per chunk
		{typ: "stco", payload: u32(0, 4, 100, 500, 900, 0)},
	}
	got := headSampleLocs(boxes, 16)
	want := []sampleLoc{{100, 10}, {110, 20}, {500, 30}, {900, 40}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d: got %v, want %v", i, got[i], want[i])
		}
	}
	if got := headSampleLocs(boxes, 1); len(got) != 1 {
		t.Errorf("n=1 gave %d samples", len(got))
	}
}

// scanCountingReader counts the reads that reach the underlying file.
type scanCountingReader struct {
	r     *bytes.Reader
	reads int
	bytes int64
}

func (c *scanCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.reads++
	c.bytes += int64(n)
	return n, err
}

func (c *scanCountingReader) Seek(off int64, whence int) (int64, error) { return c.r.Seek(off, whence) }

// TestMP4InBandScanReadsOneRun: the head samples of the fixture sit in one run,
// so the scan type costs one read beyond what the option reads without it.
func TestMP4InBandScanReadsOneRun(t *testing.T) {
	data, err := os.ReadFile("../internal/testdata/h264_mbaff_tff.mp4")
	if err != nil {
		t.Fatal(err)
	}
	run := func(scan bool) *scanCountingReader {
		cr := &scanCountingReader{r: bytes.NewReader(data)}
		c, _, err := ReadMeta(context.Background(), cr, "x.mp4", Options{InBandColour: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Tracks[0].ScanType == "interlaced"; got != scan {
			t.Fatalf("interlaced = %v, want %v", got, scan)
		}
		return cr
	}
	with := run(true)
	cr := &scanCountingReader{r: bytes.NewReader(data)}
	if _, _, err := ReadMeta(context.Background(), cr, "x.mp4"); err != nil {
		t.Fatal(err)
	}
	if extra := with.reads - cr.reads; extra > 1 {
		t.Errorf("the scan type cost %d reads, want one", extra)
	}
	if extra := with.bytes - cr.bytes; extra > inbandScanBudget {
		t.Errorf("the scan type read %d bytes, past the %d budget", extra, inbandScanBudget)
	}
}
