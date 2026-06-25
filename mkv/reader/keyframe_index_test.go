package reader

import (
	"bytes"
	"context"
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

func contains(s []int64, v int64) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
