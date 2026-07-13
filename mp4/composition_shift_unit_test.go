package mp4

// The branches the end-to-end tests never reach: the prefix rule's edges, and the
// clamp that catches a stream reordering deeper than its opening GOP announced.

import "testing"

func TestCompositionPrefixLen(t *testing.T) {
	cases := []struct {
		name string
		sync []bool
		want int
	}{
		{"GOP closes on the second keyframe", []bool{true, false, false, true, false}, 3},
		{"no second keyframe: the whole slice", []bool{true, false, false}, 3},
		{"capped when the GOP never closes", make([]bool, compositionPrefix+50), compositionPrefix},
		{"empty", nil, 0},
	}
	for _, tc := range cases {
		if got := compositionPrefixLen(tc.sync); got != tc.want {
			t.Errorf("%s: compositionPrefixLen = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestCompositionShiftTS(t *testing.T) {
	cases := []struct {
		name string
		pts  []int64
		sync []bool
		want int64
	}{
		// Decode order I P B B: the B at 42ms decodes third, after the P that
		// presents at 126 - so it decodes at 84 and would present 42ms EARLIER
		// than it decodes. One frame of hold-back, which is exactly what a real
		// 1-level encoder measures (42ms at 23.976fps).
		{"one reorder level", []int64{0, 126, 42, 84}, []bool{true, false, false, false}, 42},
		{"no reordering", []int64{0, 42, 84, 126}, []bool{true, false, false, false}, 0},
		{"a single sample cannot reorder", []int64{0}, []bool{true}, 0},
		{"nothing at all", nil, nil, 0},
	}
	for _, tc := range cases {
		if got := compositionShiftTS(tc.pts, tc.sync); got != tc.want {
			t.Errorf("%s: compositionShiftTS = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestFillFragTimingClampsDeeperReorder covers the safety branch: a stream whose
// first GOP shows one level of reordering but which later reorders deeper than the
// shift measured from it. The offsets must STILL come out non-negative - a single
// sample presenting at its decode time is a sub-frame error, whereas one negative
// offset would have a reader re-time the entire presentation.
func TestFillFragTimingClampsDeeperReorder(t *testing.T) {
	// First GOP: I P B B (shift = 42, one frame). Then a frame thrown far further
	// ahead in decode order than anything that first GOP justified.
	samples := []fragSample{
		{ptsMs: 0, sync: true}, {ptsMs: 126}, {ptsMs: 42}, {ptsMs: 84},
		{ptsMs: 168, sync: true}, {ptsMs: 500}, {ptsMs: 210}, {ptsMs: 252},
		{ptsMs: 294}, {ptsMs: 336}, {ptsMs: 378}, {ptsMs: 420},
	}
	_, hasCTS, _, shift := fillFragTiming(samples, 42, movieTimescale, 0)
	if shift != 42 {
		t.Fatalf("shift = %d, want 42 (one frame, measured over the first GOP)", shift)
	}
	if !hasCTS {
		t.Fatal("hasCTS = false on a reordered track")
	}
	for i, s := range samples {
		if s.ctsTS < 0 {
			t.Errorf("sample %d kept a negative composition offset (%d): the clamp did not hold", i, s.ctsTS)
		}
	}
}
