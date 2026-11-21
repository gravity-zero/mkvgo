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
	}
	for _, tc := range cases {
		if got := compositionShiftTS(tc.pts, tc.sync, tc.reorder); got != tc.want {
			t.Errorf("%s: compositionShiftTS = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A stream whose first GOP shows one level of reordering but which later reorders deeper, with no parameter set
// to announce it: the late frame's offset goes negative (trun version 1 is signed) and every frame keeps the
// presentation time the source gives it. Clamped to zero, it took its neighbour's and a player dropped it.
func TestFillFragTimingDeeperReorderKeepsPTS(t *testing.T) {
	// First GOP: I P B B (shift = 42, one frame). The second GOP reorders two frames deep.
	samples := []fragSample{
		{ptsMs: 0, sync: true}, {ptsMs: 126}, {ptsMs: 42}, {ptsMs: 84},
		{ptsMs: 168, sync: true}, {ptsMs: 336}, {ptsMs: 252}, {ptsMs: 210},
		{ptsMs: 294}, {ptsMs: 378}, {ptsMs: 420}, {ptsMs: 462},
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
	_, _, _, shift := fillFragTimingTS(samples, nil, 42, movieTimescale, 0, nil, 2)
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
