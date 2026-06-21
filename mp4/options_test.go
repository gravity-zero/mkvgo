package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func TestSkipUnsupportedDropsTrack(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(640), Height: u32p(480), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "truehd", Channels: u8p(8), SampleRate: f64p(48000)},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1, 2, 3}},
		{track: 2, pts: 0, key: true, data: []byte{9, 9, 9}},
		{track: 1, pts: 40, key: false, data: []byte{4, 5, 6}},
	}
	src := buildMKV(t, tracks, blocks)

	// Default: strict → error.
	dst := filepath.Join(t.TempDir(), "strict.mp4")
	if err := RemuxToMP4(context.Background(), src, dst); err == nil {
		t.Fatal("default policy should error on an unsupported track")
	}

	// SkipUnsupported: drop truehd, keep video, report the drop.
	var dropped []DroppedTrack
	dst2 := filepath.Join(t.TempDir(), "lenient.mp4")
	err := RemuxToMP4(context.Background(), src, dst2, Options{
		SkipUnsupported: true,
		OnDrop:          func(d DroppedTrack) { dropped = append(dropped, d) },
	})
	if err != nil {
		t.Fatalf("SkipUnsupported should succeed: %v", err)
	}
	if len(dropped) != 1 || dropped[0].Codec != "truehd" || dropped[0].ID != 2 {
		t.Fatalf("expected one dropped truehd track, got %+v", dropped)
	}
	data, err := os.ReadFile(dst2)
	if err != nil {
		t.Fatal(err)
	}
	traks := moovTraks(t, walkBoxes(t, data, 0))
	if len(traks) != 1 || traks[0].handler != "vide" {
		t.Fatalf("expected only the video trak after skipping truehd, got %d", len(traks))
	}
}

func TestSkipUnsupportedStillNeedsOneTrack(t *testing.T) {
	tracks := []mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "truehd", Channels: u8p(8)}}
	blocks := []genBlock{{track: 1, pts: 0, key: true, data: []byte{1}}}
	src := buildMKV(t, tracks, blocks)
	dst := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), src, dst, Options{SkipUnsupported: true}); err == nil {
		t.Fatal("expected error when every track is dropped")
	}
}

func TestBitmapSubtitleReported(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "pgs"},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1, 2}},
		{track: 2, pts: 0, key: true, data: []byte{0x14, 0x00}},
	}
	src := buildMKV(t, tracks, blocks)
	var dropped []DroppedTrack
	dst := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), src, dst, Options{OnDrop: func(d DroppedTrack) { dropped = append(dropped, d) }}); err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0].Type != mkv.SubtitleTrack || dropped[0].Codec != "pgs" {
		t.Fatalf("expected the PGS subtitle reported as dropped, got %+v", dropped)
	}
}

func TestFastStartLayoutAndOffsets(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: u8p(2), SampleRate: f64p(48000)},
	}
	var blocks []genBlock
	var wantVideo [][]byte
	vi := 0
	for ts := int64(0); ts < 200; ts += 20 {
		if ts%40 == 0 {
			d := bytes.Repeat([]byte{byte('V'), byte(vi)}, 7+vi)
			blocks = append(blocks, genBlock{track: 1, pts: ts, key: vi == 0, data: d})
			wantVideo = append(wantVideo, d)
			vi++
		}
		blocks = append(blocks, genBlock{track: 2, pts: ts, key: true, data: bytes.Repeat([]byte{byte('A'), byte(ts)}, 4)})
	}
	src := buildMKV(t, tracks, blocks)

	dst := filepath.Join(t.TempDir(), "fs.mp4")
	if err := RemuxToMP4(context.Background(), src, dst, Options{FastStart: true}); err != nil {
		t.Fatalf("RemuxToMP4 faststart: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	boxes := walkBoxes(t, data, 0)

	// Top-level order must be ftyp, moov, mdat.
	var order []string
	for _, b := range boxes {
		order = append(order, b.typ)
	}
	if len(order) < 3 || order[0] != "ftyp" || order[1] != "moov" || order[2] != "mdat" {
		t.Fatalf("fast-start box order = %v, want [ftyp moov mdat ...]", order)
	}

	// The chunk offsets in moov (now before mdat) must still resolve correctly.
	var video parsedTrack
	for _, pt := range moovTraks(t, boxes) {
		if pt.handler == "vide" {
			video = pt
		}
	}
	got := extractSamples(t, data, video)
	assertSamplesEqual(t, "video", got, wantVideo)
}

func TestColrBox(t *testing.T) {
	// HDR10: BT.2020 primaries(9) + SMPTE-2084 transfer(16) + BT.2020-NC matrix(9).
	prim, trc, mtx := uint16(9), uint16(16), uint16(9)
	rng := uint16(1) // limited
	tr := &mkv.Track{ColorPrimaries: &prim, ColorTransfer: &trc, ColorSpace: &mtx, ColorRange: &rng}
	b := colrBox(tr)
	if b == nil {
		t.Fatal("colrBox should be present when colour info is set")
	}
	want := []byte{'n', 'c', 'l', 'x', 0x00, 0x09, 0x00, 0x10, 0x00, 0x09, 0x00}
	if !bytes.Equal(b[8:], want) {
		t.Errorf("colr payload = % x\nwant         % x", b[8:], want)
	}

	// Full range sets the high bit.
	full := uint16(2)
	tr.ColorRange = &full
	if colrBox(tr)[len(colrBox(tr))-1]&0x80 == 0 {
		t.Error("full range flag should be set for ColorRange=2")
	}

	// No colour info → nil.
	if colrBox(&mkv.Track{}) != nil {
		t.Error("colrBox should be nil with no colour info")
	}
}

func TestVisualEntryIncludesColr(t *testing.T) {
	spec, _ := lookupCodec("hevc")
	prim, trc, mtx := uint16(9), uint16(16), uint16(9)
	tr := &mkv.Track{ID: 1, Codec: "hevc", CodecPrivate: []byte{0x01, 0x02},
		Width: u32p(3840), Height: u32p(2160), ColorPrimaries: &prim, ColorTransfer: &trc, ColorSpace: &mtx}
	entry, err := spec.sampleEntry(tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(entry, []byte("nclx")) {
		t.Error("HDR video sample entry should contain a colr/nclx box")
	}
	if !bytes.Contains(entry, []byte{0x00, 0x09, 0x00, 0x10, 0x00, 0x09}) {
		t.Error("colr should carry the BT.2020/SMPTE-2084 code points")
	}
}
