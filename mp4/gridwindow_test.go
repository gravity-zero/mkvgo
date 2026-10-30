package mp4

import (
	"context"
	"testing"
)

// gridCase times frames from their block timecodes, a timecode's slot being a tenth of it (a block per timecode).
func gridCase(pts []int64, span gridSpan) (dts, dur []int64) {
	const g = 1024
	dts, dur = make([]int64, len(pts)), make([]int64, len(pts))
	gridTime(len(pts), func(i int) int64 { return pts[i] }, func(ms int64) int64 { return ms / 10 }, g, span, func(i int, d, u int64) {
		dts[i], dur[i] = d, u
	})
	return dts, dur
}

// TestGridTimeGivesTheDelayBack: a window on the grid is timed one frame per slot; one running a frame late (a
// timecode ahead, then back on its slot) ends where the next window opens, every frame from the late one shorter,
// decode times rising; a lace's frames take one slot each; a window with more frames than ticks keeps its numbering.
func TestGridTimeGivesTheDelayBack(t *testing.T) {
	const g = 1024
	check := func(name string, dts, dur []int64, open, end int64) {
		t.Helper()
		if dts[0] != open*g {
			t.Errorf("%s: opens at %d, want %d", name, dts[0], open*g)
		}
		for i := 1; i < len(dts); i++ {
			if dts[i] <= dts[i-1] || dts[i-1]+dur[i-1] != dts[i] {
				t.Errorf("%s: frame %d at %d after %d lasting %d", name, i, dts[i], dts[i-1], dur[i-1])
			}
		}
		if last := len(dts) - 1; dts[last]+dur[last] != end*g || dur[last] <= 0 {
			t.Errorf("%s: ends at %d, want %d", name, dts[last]+dur[last], end*g)
		}
	}

	dts, dur := gridCase([]int64{100, 110, 120, 130}, gridSpan{-1, 14})
	check("on the grid", dts, dur, 10, 14)
	for i := range dur {
		if dur[i] != g {
			t.Errorf("on the grid: frame %d lasts %d", i, dur[i])
		}
	}

	// The frame of slot 12 is stored a slot ahead, the next back on its own: numbered 13, 14, 15, 16, one late.
	dts, dur = gridCase([]int64{100, 110, 130, 131, 140, 150}, gridSpan{-1, 16})
	check("ahead, given back", dts, dur, 10, 16)
	if dts[2] != 13*g {
		t.Errorf("ahead: the frame ahead moved to %d, want %d", dts[2], 13*g)
	}
	for i := 3; i < len(dur); i++ {
		if dur[i] >= g {
			t.Errorf("ahead: frame %d past the late one lasts %d, want under %d", i, dur[i], g)
		}
	}

	// The last frame comes back on the slot before it: late on the window's last frame, given back from the one before.
	dts, dur = gridCase([]int64{100, 110, 120, 125}, gridSpan{-1, 13})
	check("late on its last frame", dts, dur, 10, 13)

	dts, dur = gridCase([]int64{100, 110, 120}, gridSpan{11, 13})
	check("late from its opening", dts, dur, 11, 13)

	dts, dur = gridCase([]int64{100, 100, 100, 130}, gridSpan{-1, -1})
	check("a lace, then the track's last frame", dts, dur, 10, 14)

	dts, dur = gridCase([]int64{100, 110, 120, 130}, gridSpan{-1, 15})
	check("a gap after the last frame", dts, dur, 10, 15)
	if dur[3] != 2*g {
		t.Errorf("gap: the last frame lasts %d, want %d", dur[3], 2*g)
	}

	dts, dur = gridCase([]int64{100, 110, 120, 130}, gridSpan{-1, 10})
	if dur[3] != 1 || dts[3] != 13*g {
		t.Errorf("more frames than room: last frame at %d lasting %d, want its slot and one tick", dts[3], dur[3])
	}
}

// TestGridWindowsOnTheirTimecodesAfterARunAhead: the full pass, window by window, numbers a run stored a frame ahead
// as the source has it, then puts every frame after the window it ends in back on its own slot (it stayed a frame
// late to the end of the track); on the grid, it is the whole-track numbering.
func TestGridWindowsOnTheirTimecodesAfterARunAhead(t *testing.T) {
	const g = 1024
	scale := tsScale(44100)
	build := func(ahead bool) []fragSample {
		var s []fragSample
		for i := int64(0); i*1024*1000/44100 < 12000; i++ {
			ms := i * 1024 * 1000 / 44100
			if ahead && ms >= 3700 && ms < 3950 {
				ms += 23
			}
			s = append(s, fragSample{blockPtsMs: ms, ptsMs: ms, size: 1, sync: true})
		}
		return s
	}
	bounds := []int64{0, 2000, 4000, 6000, 8000, 10000}

	clean, whole := build(false), build(false)
	fillFragTimingTS(clean, nil, 0, 44100, g, bounds)
	fillFragTimingTS(whole, nil, 0, 44100, g, nil)
	for i := range clean {
		if clean[i].dtsTS != whole[i].dtsTS || clean[i].durTS != whole[i].durTS {
			t.Fatalf("on the grid: frame %d timed %d+%d by window, %d+%d whole", i, clean[i].dtsTS, clean[i].durTS, whole[i].dtsTS, whole[i].durTS)
		}
	}

	s := build(true)
	_, _, total, _ := fillFragTimingTS(s, nil, 0, 44100, g, bounds)
	anchor := scale(s[0].blockPtsMs)
	for i := range s {
		if s[i].blockPtsMs < 4000 {
			continue
		}
		if want := gridIndex(scale(s[i].blockPtsMs)-anchor, g) * g; s[i].dtsTS != want {
			t.Fatalf("frame %d at %d ms: decode time %d, want its own slot %d", i, s[i].blockPtsMs, s[i].dtsTS, want)
		}
	}
	if want := int64(len(s)) * g; total != want {
		t.Errorf("the track lasts %d, want %d frames' worth (%d)", total, len(s), want)
	}
}

// TestGridTrackDurationPlanEqualsFullPass: the plan's track duration is the sum of the full pass's, on a run ahead of
// the grid and on one behind it.
func TestGridTrackDurationPlanEqualsFullPass(t *testing.T) {
	ctx := context.Background()
	for _, src := range []struct{ name, path string }{{"ahead", transientGridSource(t)}, {"behind", jitteredGridSource(t)}} {
		o := Options{SegmentMs: 2000}
		res, err := remuxToHLSInto(ctx, src.path, t.TempDir(), &o)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := PlanHLS(ctx, src.path, Options{SegmentMs: 2000})
		if err != nil {
			t.Fatal(err)
		}
		for i, ft := range res.fts {
			if got := plan.tracks[i].ft.durMediaTS; got != ft.durMediaTS {
				t.Errorf("%s track %d: the plan says %d, the full pass %d", src.name, i, got, ft.durMediaTS)
			}
		}
	}
}
