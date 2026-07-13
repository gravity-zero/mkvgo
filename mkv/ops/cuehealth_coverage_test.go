package ops

// cuehealth_coverage_test.go pins the verdict on what actually serves a seek:
// the VIDEO cues' coverage. Cues keyed on another track are inert (the keyframe
// index drops them), so their share - however large - must not condemn a file.
// The shapes below are the ones measured on a real library.

import (
	"context"
	"math"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// cueSpread returns n cues on track trk, every stepMs.
func cueSpread(trk uint64, n int, stepMs int64) []mkv.CuePoint {
	out := make([]mkv.CuePoint, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, mkv.CuePoint{TimeMs: int64(i) * stepMs, Track: trk, ClusterPos: int64(100 + i)})
	}
	return out
}

// TestCueHealthRealWorldShapes covers the two real layouts that a zero-tolerance
// rule on non-video cues wrongly condemned: a film whose muxer let a handful of
// audio cues slip in (0.5%), and an episode cued on EVERY track, where the audio
// cues outnumber the video ones ten to one (91.7%). Both carry a dense video
// index and seek perfectly, so both are healthy.
func TestCueHealthRealWorldShapes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	const durationMs = 600_000

	cases := []struct {
		name           string
		video, audio   int
		stepV, stepA   int64
		wantNonVideoPc float64
	}{
		// 2000 video cues every 300ms; 10 stray audio cues -> 0.5% non-video.
		{"film with a few stray audio cues", 2000, 10, 300, 60_000, 0.5},
		// Every track cued: 3000 video every 200ms, 33000 audio -> 91.7% non-video.
		{"every track cued, audio dominates", 3000, 33_000, 200, 18, 91.7},
	}
	for _, tc := range cases {
		cues := append(cueSpread(1, tc.video, tc.stepV), cueSpread(2, tc.audio, tc.stepA)...)
		path := buildMKVWithCuesDur(t, dir, tc.name+".mkv", tracks, cueHealthFixtureSets(4), cues, durationMs)

		r, err := CueHealth(ctx, path)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !r.Healthy {
			t.Errorf("%s: Healthy = false (%s) - the video index is dense (%d cues, worst hole %dms), the audio cues are inert",
				tc.name, r.Reason, r.VideoCues, r.MaxVideoGapMs)
		}
		if math.Abs(r.NonVideoPct-tc.wantNonVideoPc) > 0.2 {
			t.Errorf("%s: NonVideoPct = %.1f, want ~%.1f (still reported, just not condemning)", tc.name, r.NonVideoPct, tc.wantNonVideoPc)
		}
	}
}

// TestCueHealthSparseVideoIndex is the angle the coverage rule keeps closed: the
// cues ARE video-keyed (nothing misskeyed, nothing stale) but so far apart that a
// seek into the hole lands a minute from its target. That is not a usable index.
func TestCueHealthSparseVideoIndex(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}

	// 10 video cues, one per minute, over a 10-minute file: 60s holes.
	path := buildMKVWithCuesDur(t, dir, "sparse.mkv", tracks, cueHealthFixtureSets(4),
		cueSpread(1, 10, 60_000), 600_000)

	r, err := CueHealth(ctx, path)
	if err != nil {
		t.Fatalf("CueHealth: %v", err)
	}
	if r.Healthy {
		t.Errorf("Healthy = true: 60s holes in the video cues are not seekable (%+v)", r)
	}
	if r.MaxVideoGapMs != 60_000 {
		t.Errorf("MaxVideoGapMs = %d, want 60000", r.MaxVideoGapMs)
	}

	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if hasFinding(d, "index-sparse") == nil {
		t.Errorf("Diagnose findings = %+v, want an index-sparse finding", d.Findings)
	}
}

// TestCueHealthTailHoleCountsAsGap covers the half-indexed file: the video cues
// are dense but stop halfway, so every seek into the second half lands on the
// last cue. The hole between the last cue and the declared duration counts.
func TestCueHealthTailHoleCountsAsGap(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}

	// 60 video cues every second, then nothing for the remaining 9 minutes.
	path := buildMKVWithCuesDur(t, dir, "halfindexed.mkv", tracks, cueHealthFixtureSets(4),
		cueSpread(1, 60, 1000), 600_000)

	r, err := CueHealth(ctx, path)
	if err != nil {
		t.Fatalf("CueHealth: %v", err)
	}
	if r.Healthy {
		t.Errorf("Healthy = true: the index stops at %dms of a %dms file (%+v)", r.LastCueMs, 600_000, r)
	}
	if want := int64(600_000 - 59_000); r.MaxVideoGapMs != want {
		t.Errorf("MaxVideoGapMs = %d, want %d (last cue to duration)", r.MaxVideoGapMs, want)
	}
}

// TestCueHealthAudioKeyedIsStillMisskeyed guards the defect the check was built
// for: an index with no video cue at all, on a file that has a video track.
func TestCueHealthAudioKeyedIsStillMisskeyed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}

	path := buildMKVWithCuesDur(t, dir, "audiokeyed.mkv", tracks, cueHealthFixtureSets(4),
		cueSpread(2, 300, 1000), 600_000)

	r, err := CueHealth(ctx, path)
	if err != nil {
		t.Fatalf("CueHealth: %v", err)
	}
	if r.Healthy || r.VideoCues != 0 {
		t.Errorf("an index with no video cue must stay unhealthy: %+v", r)
	}

	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if hasFinding(d, "index-misskeyed") == nil {
		t.Errorf("Diagnose findings = %+v, want index-misskeyed", d.Findings)
	}
}
