package mp4

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// buildForensicSource builds an H.264-shaped source whose samples tile as
// 4-byte-length-prefixed NALs: an IDR opens each GOP (type 5, ref), most
// frames are referenced P slices (type 1, nal_ref_idc 2), and every third
// non-key frame is disposable (type 1, nal_ref_idc 0) - the frames a
// forensic variant may drop.
func buildForensicSource(t *testing.T, disposables bool) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	nal := func(hdr byte, tag byte) []byte {
		return []byte{0x00, 0x00, 0x00, 0x03, hdr, 0xC0, tag}
	}
	var blocks []genBlock
	for i := 0; i < 100; i++ {
		var data []byte
		switch {
		case i%25 == 0:
			data = nal(0x65, byte(i)) // IDR, nal_ref_idc=3
		case disposables && i%3 == 0:
			data = nal(0x01, byte(i)) // non-IDR, nal_ref_idc=0: disposable
		default:
			data = nal(0x41, byte(i)) // non-IDR, nal_ref_idc=2: referenced
		}
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: data})
	}
	for i := 0; i < 200; i++ {
		blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 20, key: true,
			data: []byte{0xAA, 0xBB, byte(i)}})
	}
	sortGenBlocks(blocks)
	return buildMKV(t,
		[]mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
		},
		blocks)
}

// trunDurations parses a segment and returns its per-sample durations.
func trunDurations(t *testing.T, seg []byte) []uint32 {
	t.Helper()
	fs := parseForensicSegment(seg)
	if fs == nil {
		t.Fatal("segment does not parse as a single-video-traf fMP4 segment")
	}
	return fs.durs
}

func TestForensicPlanVariants(t *testing.T) {
	ctx := context.Background()
	src := buildForensicSource(t, true)
	fp, err := PlanForensic(ctx, src, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if fp.NumSegments() < 2 {
		t.Fatalf("need >= 2 segments, got %d", fp.NumSegments())
	}

	plain, err := PlanHLS(ctx, src, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fp.MediaPlaylist(), plain.MediaPlaylist()) || !bytes.Equal(fp.InitSegment(), plain.InitSegment()) {
		t.Error("forensic manifest and init must equal the plain plan's (shared across variants)")
	}

	distinctCount := 0
	for n := 0; n < fp.NumSegments(); n++ {
		segA, err := fp.Segment(ctx, n, false)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := plain.Segment(ctx, n)
		if !bytes.Equal(segA, want) {
			t.Errorf("segment %d variant A must be the ordinary segment", n)
		}
		segB, err := fp.Segment(ctx, n, true)
		if err != nil {
			t.Fatal(err)
		}
		distinct, err := fp.Distinct(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		if !distinct {
			if !bytes.Equal(segA, segB) {
				t.Errorf("segment %d reported not distinct but variants differ", n)
			}
			continue
		}
		distinctCount++
		if bytes.Equal(segA, segB) {
			t.Errorf("segment %d reported distinct but variants are identical", n)
		}
		// Contract 1: same total duration, sample for sample-sum.
		dursA, dursB := trunDurations(t, segA), trunDurations(t, segB)
		if len(dursB) != len(dursA)-1 {
			t.Errorf("segment %d variant B must have exactly one sample fewer (%d vs %d)", n, len(dursB), len(dursA))
		}
		var sumA, sumB uint64
		for _, d := range dursA {
			sumA += uint64(d)
		}
		for _, d := range dursB {
			sumB += uint64(d)
		}
		if sumA != sumB {
			t.Errorf("segment %d durations diverge after the drop (%d vs %d ticks): timing not compensated", n, sumA, sumB)
		}
		// Contract 3: variant B still parses as a coherent segment (sizes
		// tile the mdat, offsets consistent).
		if parseForensicSegment(segB) == nil {
			t.Errorf("segment %d variant B does not re-parse as a valid segment", n)
		}
	}
	if distinctCount < 2 {
		t.Errorf("expected at least 2 distinct segments in a disposable-rich source, got %d", distinctCount)
	}

	// Pattern routing mirrors the two-encode watermark.
	got, err := fp.SegmentForPattern(ctx, 0, []byte{0x01})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := fp.Segment(ctx, 0, true)
	if !bytes.Equal(got, want) {
		t.Error("SegmentForPattern bit 0 must route to variant B")
	}
}

// TestForensicNoDisposableFrames: a source whose every frame is referenced
// has no variant to offer - Distinct is false and B equals A everywhere.
func TestForensicNoDisposableFrames(t *testing.T) {
	ctx := context.Background()
	src := buildForensicSource(t, false)
	fp, err := PlanForensic(ctx, src, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < fp.NumSegments(); n++ {
		distinct, err := fp.Distinct(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		if distinct {
			t.Errorf("segment %d reported distinct without any disposable frame", n)
		}
		segA, _ := fp.Segment(ctx, n, false)
		segB, _ := fp.Segment(ctx, n, true)
		if !bytes.Equal(segA, segB) {
			t.Errorf("segment %d variants differ without any disposable frame", n)
		}
	}
}

// TestForensicRejects: encryption is refused like the two-encode watermark.
func TestForensicRejects(t *testing.T) {
	ctx := context.Background()
	src := buildForensicSource(t, true)
	key := []byte("0123456789abcdef")
	if _, err := PlanForensic(ctx, src, Options{SegmentMs: 1000,
		Encrypt: &HLSEncryption{Key: key, KeyURI: "https://k/x"}}); err == nil {
		t.Error("forensic planning must refuse Options.Encrypt")
	}
	if _, err := PlanForensic(ctx, src, Options{SegmentMs: 1000,
		CENC: &CENCOptions{Scheme: "cenc", Key: key, KeyID: make([]byte, 16), IV: make([]byte, 8)}}); err == nil {
		t.Error("forensic planning must refuse Options.CENC")
	}
}

// TestDropNonRefSampleForeignBytes: garbage, audio segments and truncated
// inputs must come back untouched with dropped=false, never panic.
func TestDropNonRefSampleForeignBytes(t *testing.T) {
	ctx := context.Background()
	src := buildForensicSource(t, true)
	fp, err := PlanForensic(ctx, src, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}

	// An audio rendition segment has the single-traf shape but its samples
	// are not NALs: no drop.
	audioSeg, _, err := fp.p.Resource(ctx, "seg_a1_00001.m4s")
	if err != nil {
		t.Fatalf("audio rendition segment: %v", err)
	}
	if out, dropped := DropNonRefSample(audioSeg); dropped || !bytes.Equal(out, audioSeg) {
		t.Error("an audio segment must come back untouched")
	}

	for _, in := range [][]byte{nil, {}, []byte("garbage"), bytes.Repeat([]byte{0x00}, 64)} {
		if out, dropped := DropNonRefSample(in); dropped || !bytes.Equal(out, in) {
			t.Error("foreign bytes must come back untouched")
		}
	}
	// A truncated real segment must be refused, not mis-parsed.
	seg, _ := fp.Segment(ctx, 0, false)
	if out, dropped := DropNonRefSample(seg[:len(seg)/2]); dropped || !bytes.Equal(out, seg[:len(seg)/2]) {
		t.Error("a truncated segment must come back untouched")
	}
}
