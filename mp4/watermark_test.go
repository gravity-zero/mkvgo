package mp4

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// buildWatermarkPair builds two GOP-aligned variants of one title: identical
// track metadata, timeline and keyframe pattern (so their init segments and
// segment durations match), but different sample bytes (so their segments are
// forensically distinguishable). fillA/fillB tag the sample payloads.
func buildWatermarkVariant(t *testing.T, fill byte) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	for i := 0; i < 100; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, fill, byte(i)}})
	}
	for i := 0; i < 200; i++ {
		blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 20, key: true,
			data: []byte{0xAA, fill, byte(i)}})
	}
	sortGenBlocks(blocks)
	return buildMKV(t,
		[]mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
		},
		blocks)
}

func TestWatermarkPlanRoutesSegments(t *testing.T) {
	ctx := context.Background()
	srcA := buildWatermarkVariant(t, 0xA1)
	srcB := buildWatermarkVariant(t, 0xB2)

	wm, err := PlanWatermark(ctx, srcA, srcB, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if wm.NumSegments() < 2 {
		t.Fatalf("need >= 2 segments to watermark, got %d", wm.NumSegments())
	}

	// The two underlying variants must be spliceable: identical init and media
	// playlist (the manifest is shared across all sessions).
	a, _ := PlanHLS(ctx, srcA, Options{SegmentMs: 1000})
	b, _ := PlanHLS(ctx, srcB, Options{SegmentMs: 1000})
	if !bytes.Equal(a.InitSegment(), b.InitSegment()) {
		t.Fatal("test setup: variants must share an init segment")
	}
	if !bytes.Equal(wm.MediaPlaylist(), a.MediaPlaylist()) {
		t.Error("watermark media playlist must equal the shared variant playlist")
	}
	if !bytes.Equal(wm.InitSegment(), a.InitSegment()) {
		t.Error("watermark init must equal the shared variant init")
	}

	// Per-segment routing: fromB=false yields A's bytes, fromB=true yields B's,
	// and A's segment differs from B's (the forensic distinction).
	for i := 0; i < wm.NumSegments(); i++ {
		segA, err := wm.Segment(ctx, i, false)
		if err != nil {
			t.Fatal(err)
		}
		segB, err := wm.Segment(ctx, i, true)
		if err != nil {
			t.Fatal(err)
		}
		wantA, _ := a.Segment(ctx, i)
		wantB, _ := b.Segment(ctx, i)
		if !bytes.Equal(segA, wantA) {
			t.Errorf("segment %d fromB=false must equal variant A", i)
		}
		if !bytes.Equal(segB, wantB) {
			t.Errorf("segment %d fromB=true must equal variant B", i)
		}
		if bytes.Equal(segA, segB) {
			t.Errorf("segment %d is identical across variants - no forensic signal", i)
		}
	}
}

func TestWatermarkSegmentForPattern(t *testing.T) {
	ctx := context.Background()
	wm, err := PlanWatermark(ctx, buildWatermarkVariant(t, 0x01), buildWatermarkVariant(t, 0x02), Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	n := wm.NumSegments()
	// Pattern: segment 0 -> B (bit0 set), the rest -> A.
	pattern := []byte{0x01}
	for i := 0; i < n; i++ {
		got, err := wm.SegmentForPattern(ctx, i, pattern)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := wm.Segment(ctx, i, i == 0)
		if !bytes.Equal(got, want) {
			t.Errorf("segment %d: pattern routing selected the wrong variant", i)
		}
	}
}

func TestPatternBit(t *testing.T) {
	pat := []byte{0b0000_0101, 0b0000_0010} // bits 0,2 set in byte0; bit 9 set
	cases := map[int]bool{0: true, 1: false, 2: true, 3: false, 8: false, 9: true, 16: false, 100: false}
	for n, want := range cases {
		if got := patternBit(pat, n); got != want {
			t.Errorf("patternBit(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestWatermarkRejectsMisaligned(t *testing.T) {
	ctx := context.Background()
	srcA := buildWatermarkVariant(t, 0x01)
	// A shorter variant: fewer frames -> fewer segments -> not aligned.
	w, h := uint32(320), uint32(240)
	var blocks []genBlock
	for i := 0; i < 25; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	sortGenBlocks(blocks)
	srcShort := buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}, blocks)

	if _, err := PlanWatermark(ctx, srcA, srcShort, Options{SegmentMs: 1000}); err == nil {
		t.Error("misaligned variants (different segment count / init) must be rejected")
	}
}
