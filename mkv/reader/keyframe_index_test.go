package reader

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"testing"
)

// TestKeyframeIndexComplete proves the complete index collects EVERY video
// keyframe — multiple per Cluster, non-keyframes skipped, audio ignored,
// BlockGroups handled — unlike the sampled variant which takes one per Cluster.
func TestKeyframeIndexComplete(t *testing.T) {
	const v, a = 1, 2 // video / audio track numbers (see tracksElem)
	children := [][]byte{
		infoElem(), tracksElem(),
		// Two keyframes in one Cluster (0 and 66) with a non-keyframe between:
		// the complete index must report both; a per-Cluster sample only the first.
		cluster(0,
			simpleBlock(v, 0, true, 80),
			simpleBlock(v, 33, false, 80),
			simpleBlock(v, 66, true, 80),
		),
		// Audio keyframe must be ignored; only the video keyframe at 1000 counts.
		cluster(1000, simpleBlock(a, 0, true, 80), simpleBlock(v, 0, true, 80)),
		// Real keyframe is the second block (2000+50), not the Cluster start.
		cluster(2000, simpleBlock(v, 0, false, 80), simpleBlock(v, 50, true, 80)),
		// BlockGroups: unreferenced (keyframe) at 3000 and 3040, referenced
		// (non-keyframe) at 3020 dropped.
		cluster(3000,
			blockGroup(v, 0, false, 80),
			blockGroup(v, 20, true, 80),
			blockGroup(v, 40, false, 80),
		),
	}
	file := segmentMKV(children...)

	c, err := ReadMeta(context.Background(), bytes.NewReader(file), "x.mkv", WithKeyframeIndex())
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	want := []int64{0, 66, 1000, 2050, 3000, 3040}
	if !reflect.DeepEqual(c.Keyframes, want) {
		t.Errorf("complete Keyframes = %v, want %v", c.Keyframes, want)
	}

	// The sampled variant on the same file is a strict subset (one keyframe per
	// Cluster), so it must miss 66 and 3040 that the complete index reports.
	cs, err := ReadMeta(context.Background(), bytes.NewReader(file), "x.mkv", WithSampledKeyframes(200))
	if err != nil {
		t.Fatalf("ReadMeta sampled: %v", err)
	}
	if len(cs.Keyframes) >= len(c.Keyframes) {
		t.Errorf("sampled (%d) should report fewer keyframes than complete (%d)", len(cs.Keyframes), len(c.Keyframes))
	}
	for _, ms := range cs.Keyframes { // every sampled point is a real keyframe
		if !contains(c.Keyframes, ms) {
			t.Errorf("sampled keyframe %d is not in the complete index %v", ms, c.Keyframes)
		}
	}

	// A Cues-indexed file must not be scanned: the option is a no-op.
	withCues := segmentMKV(infoElem(), cuesElem(3), tracksElem(), cluster(0, simpleBlock(v, 0, true, 50)))
	base, _ := ReadMeta(context.Background(), bytes.NewReader(withCues), "x.mkv")
	opt, _ := ReadMeta(context.Background(), bytes.NewReader(withCues), "x.mkv", WithKeyframeIndex())
	if !reflect.DeepEqual(base.Keyframes, opt.Keyframes) || len(base.Keyframes) == 0 {
		t.Errorf("with Cues, WithKeyframeIndex must not change Keyframes: base=%v opt=%v", base.Keyframes, opt.Keyframes)
	}
}

// TestKeyframeIndexResync covers corruption recovery: a garbage region between
// two Clusters must not abort the pass — the walk resyncs to the next Cluster and
// keeps collecting keyframes.
func TestKeyframeIndexResync(t *testing.T) {
	const v = 1
	file := segmentMKV(
		infoElem(), tracksElem(),
		cluster(0, simpleBlock(v, 0, true, 60)),
		bytes.Repeat([]byte{0x00}, 8), // invalid VINT region between Clusters
		cluster(1000, simpleBlock(v, 0, true, 60)),
	)
	c, err := ReadMeta(context.Background(), bytes.NewReader(file), "x.mkv", WithKeyframeIndex())
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	want := []int64{0, 1000}
	if !reflect.DeepEqual(c.Keyframes, want) {
		t.Errorf("Keyframes after resync = %v, want %v", c.Keyframes, want)
	}
}

// TestSeqSkipReader locks the sequential reader behind the complete pass: a
// forward skip discards in-stream (no seek), a backward skip falls back to a real
// seek, and position queries / SeekEnd resolve correctly.
func TestSeqSkipReader(t *testing.T) {
	data := []byte("0123456789abcdef")
	s := newSeqSkipReader(bytes.NewReader(data), 0)

	buf := make([]byte, 4)
	if _, err := io.ReadFull(s, buf); err != nil || string(buf) != "0123" {
		t.Fatalf("read = %q (%v), want 0123", buf, err)
	}
	if pos, _ := s.Seek(0, io.SeekCurrent); pos != 4 {
		t.Fatalf("position query = %d, want 4", pos)
	}
	// Forward skip (discard): jump over "45".
	if pos, err := s.Seek(2, io.SeekCurrent); err != nil || pos != 6 {
		t.Fatalf("forward skip = %d (%v), want 6", pos, err)
	}
	if _, err := io.ReadFull(s, buf); err != nil || string(buf) != "6789" {
		t.Fatalf("read after skip = %q (%v), want 6789", buf, err)
	}
	// Forward SeekStart (discard): to index 12.
	if pos, err := s.Seek(12, io.SeekStart); err != nil || pos != 12 {
		t.Fatalf("forward SeekStart = %d (%v), want 12", pos, err)
	}
	if _, err := io.ReadFull(s, buf); err != nil || string(buf) != "cdef" {
		t.Fatalf("read at 12 = %q (%v), want cdef", buf, err)
	}
	// Backward SeekStart (real seek): back to index 1.
	if pos, err := s.Seek(1, io.SeekStart); err != nil || pos != 1 {
		t.Fatalf("backward SeekStart = %d (%v), want 1", pos, err)
	}
	if _, err := io.ReadFull(s, buf); err != nil || string(buf) != "1234" {
		t.Fatalf("read at 1 = %q (%v), want 1234", buf, err)
	}
	// SeekEnd resolves the size.
	if pos, err := s.Seek(0, io.SeekEnd); err != nil || pos != int64(len(data)) {
		t.Fatalf("SeekEnd = %d (%v), want %d", pos, err, len(data))
	}
}

func contains(s []int64, v int64) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
