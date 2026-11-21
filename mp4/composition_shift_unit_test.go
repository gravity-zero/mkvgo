package mp4

// The branches the end-to-end tests never reach: the prefix rule's edges, and the
// stream reordering deeper than its opening GOP announced.

import (
	"encoding/hex"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

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

// ordered is n frames in presentation order, step apart.
func ordered(step int64, n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(i) * step
	}
	return out
}

// loneKeyThenReorder is a keyframe alone, a second keyframe, then groups decoded P B B (one frame of reordering).
func loneKeyThenReorder(step int64, n int) []int64 {
	out := []int64{0, step}
	for k := int64(2); len(out) < n; k += 3 {
		out = append(out, (k+2)*step, k*step, (k+1)*step)
	}
	return out[:n]
}

func loneKeySync(n int) []bool {
	s := make([]bool, n)
	s[0], s[1] = true, true
	return s
}

// pyramid is a keyframe then groups of four decoded P B b b, a B-pyramid two frames deep.
func pyramid(step int64, n int) []int64 {
	out := []int64{0}
	for k := int64(1); len(out) < n; k += 4 {
		out = append(out, (k+3)*step, (k+1)*step, k*step, (k+2)*step)
	}
	return out[:n]
}

func TestCompositionShiftTS(t *testing.T) {
	cases := []struct {
		name    string
		pts     []int64
		sync    []bool
		reorder int
		want    int64
	}{
		// Decode order I P B B: the B at 42ms decodes third, after the P that
		// presents at 126 - so it decodes at 84 and would present 42ms EARLIER
		// than it decodes. One frame of hold-back, which is exactly what a real
		// 1-level encoder measures (42ms at 23.976fps).
		{"one reorder level", []int64{0, 126, 42, 84}, []bool{true, false, false, false}, 0, 42},
		{"no reordering", []int64{0, 42, 84, 126}, []bool{true, false, false, false}, 0, 0},
		{"a single sample cannot reorder", []int64{0}, []bool{true}, 0, 0},
		{"nothing at all", nil, nil, 0, 0},
		// The opening frames are in presentation order but the parameter sets declare two frames of reordering:
		// the shift is two of the longest frame steps (41 and 42 ms at 23.976 fps on a millisecond clock).
		{"declared depth, ordered prefix", []int64{0, 42, 83, 125}, []bool{true, false, false, false}, 2, 84},
		{"declared depth below the measure", []int64{0, 126, 42, 84}, []bool{true, false, false, false}, 1, 42},
		// The frame at 84 decodes later: its hole is not a frame duration.
		{"declared depth, a hole in the prefix", []int64{0, 126, 42}, []bool{true, false, false}, 2, 84},
		{"declared depth, native ticks", []int64{0, 3754, 7508, 11261}, []bool{true, false, false, false}, 2, 7508},
		// A first GOP of one frame shows nothing: the frames after it are measured (two keyframes, then I P B B).
		{"one-frame first GOP", loneKeyThenReorder(42, 30), loneKeySync(30), 0, 42},
		// Past compositionPrefix nothing is read: a reordering that starts there is not measured.
		{"reordering past the prefix", append(ordered(42, 26), 1176, 1092, 1134), make([]bool, 29), 0, 0},
		// A first GOP longer than the prefix keeps what its 24 frames measured, as before (one frame over the two
		// the pyramid needs: the prefix ends mid-group).
		{"long first GOP keeps the prefix measure", pyramid(42, 28), append([]bool{true}, make([]bool, 27)...), 0, 126},
		// The same frames behind a one-frame first GOP are measured over their complete part: exactly two frames.
		{"complete part is not inflated", append([]int64{-42}, pyramid(42, 28)...), append([]bool{true, true}, make([]bool, 27)...), 0, 84},
	}
	for _, tc := range cases {
		if got := compositionShiftTS(tc.pts, tc.sync, tc.reorder); got != tc.want {
			t.Errorf("%s: compositionShiftTS = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A stream whose opening frames show one level of reordering but which later reorders deeper, with no parameter set
// to announce it: the late frame's offset goes negative (trun version 1 is signed) and every frame keeps the
// presentation time the source gives it. Clamped to zero, it took its neighbour's and a player dropped it.
func TestFillFragTimingDeeperReorderKeepsPTS(t *testing.T) {
	// First GOP: I P B B (shift = 42, one frame), then frames in order past the measured prefix, then a group
	// reordered two frames deep.
	samples := []fragSample{{ptsMs: 0, sync: true}, {ptsMs: 126}, {ptsMs: 42}, {ptsMs: 84}}
	for i := 4; i < 30; i++ {
		samples = append(samples, fragSample{ptsMs: int64(i) * 42, sync: i == 4})
	}
	for _, k := range []int64{33, 31, 30, 32, 34, 35} {
		samples = append(samples, fragSample{ptsMs: k * 42})
	}
	_, hasCTS, _, shift := fillFragTiming(samples, 42, movieTimescale, 0)
	if shift != 42 {
		t.Fatalf("shift = %d, want 42 (one frame, measured over the first GOP)", shift)
	}
	if !hasCTS {
		t.Fatal("hasCTS = false on a reordered track")
	}
	negative := 0
	for i, s := range samples {
		if s.ctsTS < 0 {
			negative++
		}
		if pts := s.dtsTS + int64(s.ctsTS) - shift; pts != s.ptsMs {
			t.Errorf("sample %d presents at %d, want %d", i, pts, s.ptsMs)
		}
	}
	if negative == 0 {
		t.Error("no offset went negative: the fixture does not reorder deeper than its first GOP")
	}
}

// The records are the ones libx264 and libx265 wrote for 320x180 encodes; the expected depths are the has_b_frames
// an independent parser reports for the same files.
func TestDeclaredReorderDepth(t *testing.T) {
	unhex := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	x264Pyramid := unhex("0164000cffe100186764000cacd941419f9f011000003e90000bb800f142996001000668ebe3cb22c0fdf8f800")
	x264NoB := unhex("0164000cffe100176764000cacb60a0cfcf808800001f480005dc0078a157001000668eac3cb22c0fdf8f800")
	x265 := unhex("0101600000009000000000003cf000fcfdf8f800000f03200001001840010c01ffff01600000030090000003000003003c959809" +
		"210001002942010101600000030090000003000003003ca00a080b9f796566924caf01680800001f480002ee0040" +
		"22000100074401c172b46240")
	cases := []struct {
		name  string
		track mkv.Track
		want  int
	}{
		{"x264 B-pyramid", mkv.Track{Codec: "h264", CodecPrivate: x264Pyramid}, 2},
		{"x264 without B-frames", mkv.Track{Codec: "h264", CodecPrivate: x264NoB}, 0},
		{"x265 B-pyramid", mkv.Track{Codec: "hevc", CodecPrivate: x265}, 2},
		{"no record", mkv.Track{Codec: "h264"}, 0},
		{"a truncated record", mkv.Track{Codec: "h264", CodecPrivate: x264Pyramid[:20]}, 0},
		{"another codec", mkv.Track{Codec: "vp9", CodecPrivate: x264Pyramid}, 0},
	}
	for _, tc := range cases {
		if got := declaredReorderDepth(&tc.track); got != tc.want {
			t.Errorf("%s: declaredReorderDepth = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A stream whose opening frames are in presentation order and which reorders two frames deep later, as its parameter
// sets declare: every frame keeps its own presentation time, none is clamped onto its decode time.
func TestFillFragTimingLateReorderKeepsEveryPTS(t *testing.T) {
	var samples []fragSample
	for i := 0; i < 30; i++ {
		samples = append(samples, fragSample{ptsMs: int64(i) * 1001 / 24, sync: i == 0})
	}
	for g := 0; g < 5; g++ { // decode order P B b b, a pyramid
		for _, k := range []int{3, 1, 0, 2} {
			samples = append(samples, fragSample{ptsMs: int64(30+4*g+k) * 1001 / 24})
		}
	}
	_, _, _, shift := fillFragTimingTS(samples, nil, 42, movieTimescale, 0, nil, 2, false, false)
	if shift != 84 {
		t.Fatalf("shift = %d, want 84 (two frames)", shift)
	}
	seen := map[int64]int{}
	for i, s := range samples {
		pts := s.dtsTS + int64(s.ctsTS) - shift
		if pts != s.ptsMs {
			t.Errorf("sample %d presents at %d, want %d", i, pts, s.ptsMs)
		}
		if j, dup := seen[pts]; dup {
			t.Errorf("samples %d and %d share the presentation time %d", j, i, pts)
		}
		seen[pts] = i
	}
}

// FuzzDeclaredReorderDepth: a parameter set record of any bytes gives a depth within the codecs' bound, never a panic.
func FuzzDeclaredReorderDepth(f *testing.F) {
	seed, _ := hex.DecodeString("0164000cffe100186764000cacd941419f9f011000003e90000bb800f142996001000668ebe3cb22c0fdf8f800")
	f.Add(seed, false)
	f.Add(seed, true)
	f.Fuzz(func(t *testing.T, private []byte, hevc bool) {
		tr := mkv.Track{Codec: "h264", CodecPrivate: private}
		if hevc {
			tr.Codec = "hevc"
		}
		if d := declaredReorderDepth(&tr); d < 0 || d > maxReorderDepth {
			t.Fatalf("declaredReorderDepth = %d", d)
		}
	})
}

// A sample that presents before more than maxReorderDepth samples decoded ahead of it carries a broken timestamp: it is
// flagged and takes the latest time seen. A frame reordered across a long frame duration is not one - what counts is
// how many frames it is reordered across, not how long they last.
func TestStrayTimestamps(t *testing.T) {
	ordered := func(n int) []int64 {
		out := make([]int64, n)
		for i := range out {
			out[i] = int64(i) * 40
		}
		return out
	}
	if got := strayTimestamps(ordered(100)); got != nil {
		t.Errorf("frames in order: flagged %v", got)
	}

	late := append(ordered(60), 100) // decoded last, stamped 2.3 s back: 57 frames decoded ahead of it present later
	stray := strayTimestamps(late)
	if stray == nil || !stray[60] || late[60] != 59*40 {
		t.Errorf("stray stamp: flagged=%v, rewritten to %d, want flagged and %d", stray != nil && stray[60], late[60], 59*40)
	}
	for i := 0; i < 60; i++ {
		if stray[i] || late[i] != int64(i)*40 {
			t.Fatalf("sample %d was touched: flagged=%v time=%d", i, stray[i], late[i])
		}
	}

	deep := ordered(40) // the deepest reordering there is: one frame presenting before the 16 decoded ahead of it
	deep = append(deep, 2000, 2040, 2080, 2120, 2160, 2200, 2240, 2280, 2320, 2360, 2400, 2440, 2480, 2520, 2560, 2600, 1960)
	if got := strayTimestamps(deep); got != nil {
		t.Errorf("16 frames of reordering: flagged %v", got)
	}

	gap := append(ordered(30), 5000, 3000) // an anchor after a long frame, then the B-frame two seconds before it
	if got := strayTimestamps(gap); got != nil {
		t.Errorf("one frame of reordering across a long frame duration: flagged %v", got)
	}
}

// A B-frame across a long frame duration keeps its presentation time, however far before its anchor it presents.
func TestFillFragTimingLongFrameReorderKeepsPTS(t *testing.T) {
	var samples []fragSample
	for i := 0; i < 30; i++ {
		samples = append(samples, fragSample{ptsMs: int64(i) * 40, sync: i == 0})
	}
	samples = append(samples, fragSample{ptsMs: 5000}, fragSample{ptsMs: 3000}, fragSample{ptsMs: 5040})
	_, _, _, shift := fillFragTiming(samples, 40, movieTimescale, 0)
	seen := map[int64]bool{}
	for i, s := range samples {
		pts := s.dtsTS + int64(s.ctsTS) - shift
		if pts != s.ptsMs {
			t.Errorf("sample %d presents at %d, want %d", i, pts, s.ptsMs)
		}
		if seen[pts] {
			t.Errorf("sample %d shares the presentation time %d", i, pts)
		}
		seen[pts] = true
	}
}
