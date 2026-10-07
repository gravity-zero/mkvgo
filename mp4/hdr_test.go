package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestHDRStaticMP4 covers reading HDR10 static metadata from a visual sample
// entry's clli (Content Light Level) and mdcv (Mastering Display, SMPTE ST 2086)
// boxes, including the mdcv G,B,R primary order and fixed-point → unit conversion.
func TestHDRStaticMP4(t *testing.T) {
	clli := box("clli", []byte{0x03, 0xE8, 0x01, 0x90}) // MaxCLL=1000, MaxFALL=400

	mdcv := make([]byte, 24)
	put16 := func(off int, v uint16) { binary.BigEndian.PutUint16(mdcv[off:], v) }
	put32 := func(off int, v uint32) { binary.BigEndian.PutUint32(mdcv[off:], v) }
	// Primaries G, B, R (chromaticity × 50000); white point; max/min lum (× 10000).
	put16(0, 8500)
	put16(2, 39850) // green 0.170, 0.797
	put16(4, 6550)
	put16(6, 2300) // blue 0.131, 0.046
	put16(8, 35400)
	put16(10, 14600) // red 0.708, 0.292
	put16(12, 15635)
	put16(14, 16450) // white 0.3127, 0.3290
	put32(16, 10_000_000)
	put32(20, 50) // lum 1000, 0.005

	payload := make([]byte, 78) // visual sample entry fixed header
	payload = append(payload, box("hvcC", []byte{1, 2, 3})...)
	payload = append(payload, clli...)
	payload = append(payload, box("mdcv", mdcv)...)

	var tr inTrack
	parseHDRStatic(&tr, payload, 78)

	if tr.hdr == nil {
		t.Fatal("hdr is nil, want populated")
	}
	if tr.hdr.MaxCLL != 1000 || tr.hdr.MaxFALL != 400 {
		t.Errorf("MaxCLL/MaxFALL = %d/%d, want 1000/400", tr.hdr.MaxCLL, tr.hdr.MaxFALL)
	}
	md := tr.hdr.MasteringDisplay
	if md == nil {
		t.Fatal("MasteringDisplay is nil")
	}
	for _, tc := range []struct {
		name      string
		got, want float64
	}{
		{"RedX", md.RedX, 0.708}, {"RedY", md.RedY, 0.292},
		{"GreenX", md.GreenX, 0.170}, {"GreenY", md.GreenY, 0.797},
		{"BlueX", md.BlueX, 0.131}, {"BlueY", md.BlueY, 0.046},
		{"WhiteX", md.WhiteX, 0.3127}, {"WhiteY", md.WhiteY, 0.3290},
		{"LumMax", md.LuminanceMax, 1000.0}, {"LumMin", md.LuminanceMin, 0.005},
	} {
		if math.Abs(tc.got-tc.want) > 1e-6 {
			t.Errorf("%s = %g, want %g", tc.name, tc.got, tc.want)
		}
	}
}

var hdr10Mastering = &mkv.MasteringDisplay{
	RedX: 0.68, RedY: 0.32, GreenX: 0.265, GreenY: 0.69, BlueX: 0.15, BlueY: 0.06,
	WhiteX: 0.3127, WhiteY: 0.329, LuminanceMax: 1000, LuminanceMin: 0.0001,
}

func hdrVideoTrack(codec string, private []byte, hdr *mkv.HDRStaticMetadata) mkv.Track {
	pri, tr, mat, rng := uint16(9), uint16(16), uint16(9), uint16(1)
	return mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: codec, CodecPrivate: private,
		Width: u32p(3840), Height: u32p(2160), FrameRate: f64p(24),
		ColorPrimaries: &pri, ColorTransfer: &tr, ColorSpace: &mat, ColorRange: &rng, HDR: hdr}
}

// hdrSampleEntryChildren returns the child boxes of the first video sample entry.
func hdrSampleEntryChildren(t *testing.T, file []byte) []tbox {
	t.Helper()
	moov := mustBox(t, walkBoxes(t, file, 0), "moov")
	for _, trak := range walkBoxes(t, moov.payload, 0) {
		if trak.typ != "trak" {
			continue
		}
		mdia := mustBox(t, walkBoxes(t, trak.payload, 0), "mdia")
		minf := mustBox(t, walkBoxes(t, mdia.payload, 0), "minf")
		if _, video := findBox(walkBoxes(t, minf.payload, 0), "vmhd"); !video {
			continue
		}
		stbl := mustBox(t, walkBoxes(t, minf.payload, 0), "stbl")
		stsd := mustBox(t, walkBoxes(t, stbl.payload, 0), "stsd")
		entry := walkBoxes(t, stsd.payload[8:], 0)[0] // version/flags + entry_count
		return walkBoxes(t, entry.payload[78:], 0)    // VisualSampleEntry fixed fields
	}
	t.Fatal("no video track")
	return nil
}

func TestHDRStaticBoxesWritten(t *testing.T) {
	cases := []struct {
		name string
		hdr  *mkv.HDRStaticMetadata
		mdcv bool
		clli []byte
	}{
		{"mastering display and light level", &mkv.HDRStaticMetadata{MaxCLL: 1000, MaxFALL: 400, MasteringDisplay: hdr10Mastering}, true, []byte{0x03, 0xE8, 0x01, 0x90}},
		{"mastering display only", &mkv.HDRStaticMetadata{MasteringDisplay: hdr10Mastering}, true, nil},
		{"light level only", &mkv.HDRStaticMetadata{MaxCLL: 1000, MaxFALL: 400}, false, []byte{0x03, 0xE8, 0x01, 0x90}},
		{"MaxCLL only, MaxFALL unknown is 0", &mkv.HDRStaticMetadata{MaxCLL: 600}, false, []byte{0x02, 0x58, 0x00, 0x00}},
		{"partial mastering display", &mkv.HDRStaticMetadata{MasteringDisplay: &mkv.MasteringDisplay{WhiteX: 0.3127, WhiteY: 0.329, LuminanceMax: 1000}}, false, nil},
		{"colr only", nil, false, nil},
	}
	codecs := []struct {
		name, codec string
		private     []byte
	}{{"av1", "av1", fakeAV1C}, {"h264", "h264", fakeAVCC}}
	for _, cc := range codecs {
		for _, tc := range cases {
			t.Run(cc.name+"/"+tc.name, func(t *testing.T) {
				src := buildMKV(t, []mkv.Track{hdrVideoTrack(cc.codec, cc.private, tc.hdr)},
					[]genBlock{{track: 1, pts: 0, key: true, data: []byte{1}}, {track: 1, pts: 1000, key: true, data: []byte{2}}})
				dir := t.TempDir()
				mp4Path := filepath.Join(dir, "out.mp4")
				if err := RemuxToMP4(context.Background(), src, mp4Path); err != nil {
					t.Fatalf("RemuxToMP4: %v", err)
				}
				progressive, err := os.ReadFile(mp4Path)
				if err != nil {
					t.Fatal(err)
				}
				plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
				if err != nil {
					t.Fatalf("PlanHLS: %v", err)
				}
				for _, out := range []struct {
					name string
					file []byte
				}{{"progressive MP4", progressive}, {"fMP4 init", plan.InitSegment()}} {
					children := hdrSampleEntryChildren(t, out.file)
					if _, ok := findBox(children, "colr"); !ok {
						t.Errorf("%s: colr missing", out.name)
					}
					mdcv, gotMdcv := findBox(children, "mdcv")
					if gotMdcv != tc.mdcv {
						t.Errorf("%s: mdcv present = %v, want %v", out.name, gotMdcv, tc.mdcv)
					}
					if gotMdcv {
						checkMdcv(t, out.name, mdcv.payload)
					}
					clli, gotClli := findBox(children, "clli")
					if gotClli != (tc.clli != nil) {
						t.Errorf("%s: clli present = %v, want %v", out.name, gotClli, tc.clli != nil)
					}
					if gotClli && !bytes.Equal(clli.payload, tc.clli) {
						t.Errorf("%s: clli = % x, want % x", out.name, clli.payload, tc.clli)
					}
				}
			})
		}
	}
}

func checkMdcv(t *testing.T, stage string, b []byte) {
	t.Helper()
	if len(b) != 24 {
		t.Fatalf("%s: mdcv payload is %d bytes, want 24", stage, len(b))
	}
	want16 := []uint16{13250, 34500, 7500, 3000, 34000, 16000, 15635, 16450}
	for i, w := range want16 {
		if got := binary.BigEndian.Uint16(b[2*i:]); got != w {
			t.Errorf("%s: mdcv field %d = %d, want %d", stage, i, got, w)
		}
	}
	if max, min := binary.BigEndian.Uint32(b[16:]), binary.BigEndian.Uint32(b[20:]); max != 10_000_000 || min != 1 {
		t.Errorf("%s: mdcv luminance = %d/%d, want 10000000/1", stage, max, min)
	}
}

func TestHDRStaticRemuxRoundTrip(t *testing.T) {
	want := &mkv.HDRStaticMetadata{MaxCLL: 1000, MaxFALL: 400, MasteringDisplay: hdr10Mastering}
	src := buildMKV(t, []mkv.Track{hdrVideoTrack("av1", fakeAV1C, want)},
		[]genBlock{{track: 1, pts: 0, key: true, data: []byte{1}}})
	dir := t.TempDir()
	mp4Path := filepath.Join(dir, "hdr.mp4")
	if err := RemuxToMP4(context.Background(), src, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	c, _, err := OpenMeta(context.Background(), mp4Path)
	if err != nil {
		t.Fatalf("OpenMeta: %v", err)
	}
	checkHDRStatic(t, "MP4", c.Tracks[0].HDR, want)

	backMKV := filepath.Join(dir, "back.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, backMKV); err != nil {
		t.Fatalf("RemuxFromMP4: %v", err)
	}
	full, _ := readMKV(t, backMKV)
	checkHDRStatic(t, "round-tripped MKV", full.Tracks[0].HDR, want)
}

func checkHDRStatic(t *testing.T, stage string, got, want *mkv.HDRStaticMetadata) {
	t.Helper()
	if got == nil || got.MaxCLL != want.MaxCLL || got.MaxFALL != want.MaxFALL || got.MasteringDisplay == nil {
		t.Fatalf("%s: HDR = %+v, want %+v", stage, got, want)
	}
	g, w := got.MasteringDisplay, want.MasteringDisplay
	for _, f := range []struct {
		name      string
		got, want float64
		tol       float64
	}{
		{"RedX", g.RedX, w.RedX, 1e-5}, {"RedY", g.RedY, w.RedY, 1e-5},
		{"GreenX", g.GreenX, w.GreenX, 1e-5}, {"GreenY", g.GreenY, w.GreenY, 1e-5},
		{"BlueX", g.BlueX, w.BlueX, 1e-5}, {"BlueY", g.BlueY, w.BlueY, 1e-5},
		{"WhiteX", g.WhiteX, w.WhiteX, 1e-5}, {"WhiteY", g.WhiteY, w.WhiteY, 1e-5},
		{"LuminanceMax", g.LuminanceMax, w.LuminanceMax, 5e-5}, {"LuminanceMin", g.LuminanceMin, w.LuminanceMin, 5e-5},
	} {
		if math.Abs(f.got-f.want) > f.tol {
			t.Errorf("%s: %s = %v, want %v", stage, f.name, f.got, f.want)
		}
	}
}
