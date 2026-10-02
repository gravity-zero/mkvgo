package mp4

import (
	"encoding/binary"
	"math"
	"testing"
)

// sttsBox builds an stts box from (sample_count, sample_delta) pairs.
func sttsBox(entries ...[2]uint32) []memBox {
	p := make([]byte, 8+8*len(entries))
	binary.BigEndian.PutUint32(p[4:], uint32(len(entries)))
	for i, e := range entries {
		binary.BigEndian.PutUint32(p[8+8*i:], e[0])
		binary.BigEndian.PutUint32(p[12+8*i:], e[1])
	}
	return []memBox{{typ: "stts", payload: p}}
}

// TestFrameRateOfARoundedConstantStts: 24 fps on a millisecond timescale is
// stored as deltas of 42 and 41. Read from the first entry alone the track was
// reported at 23.81 fps, and an MKV written from it carried no DefaultDuration
// at all - a demuxer then guessed 500/21 fps and complained about the
// timestamps. The rate is the samples over the time they cover; a table whose
// deltas differ by more than a tick is a variable rate and keeps the old
// reading, with no DefaultDuration.
func TestFrameRateOfARoundedConstantStts(t *testing.T) {
	for name, tc := range map[string]struct {
		stts      []memBox
		timescale uint32
		fps       float64
		durNs     int64
	}{
		"24 fps in milliseconds": {sttsBox([2]uint32{1, 42}, [2]uint32{1, 41}, [2]uint32{2, 42}, [2]uint32{1, 41}, [2]uint32{1, 42}), 1000, 24, 41666667},
		"single entry":           {sttsBox([2]uint32{100, 1001}), 24000, 24000.0 / 1001, 41708333},
		"variable rate":          {sttsBox([2]uint32{10, 40}, [2]uint32{10, 80}), 1000, 25, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if fps := headerFrameRate(tc.stts, tc.timescale); math.Abs(fps-tc.fps) > 1e-9 {
				t.Errorf("frame rate = %.6f, want %.6f", fps, tc.fps)
			}
			if ns := headerVideoFrameDurNs(tc.stts, tc.timescale); ns != tc.durNs {
				t.Errorf("frame duration = %d ns, want %d", ns, tc.durNs)
			}
		})
	}
}

// roundedStts builds the stts of n frames at num/den fps on a timescale too
// coarse for it: each decode time rounded to the tick, deltas run-length coded.
func roundedStts(n int, num, den uint64, timescale uint32) []memBox {
	var entries [][2]uint32
	prev := uint64(0)
	for i := 1; i <= n; i++ {
		t := (uint64(i)*den*uint64(timescale) + num/2) / num
		d := uint32(t - prev)
		prev = t
		if k := len(entries); k > 0 && entries[k-1][1] == d {
			entries[k-1][0]++
			continue
		}
		entries = append(entries, [2]uint32{1, d})
	}
	return sttsBox(entries...)
}

// TestStandardFrameRateFromARoundedStts: a long track pins its rate down. The
// average of millisecond deltas lands a few nanoseconds off the true frame
// duration, and a demuxer then derives another fraction than the source's
// (9998/417 for 24000/1001 on a real 23.976 fps film). When exactly one
// standard rate fits the measurement, its exact duration is used.
func TestStandardFrameRateFromARoundedStts(t *testing.T) {
	for name, tc := range map[string]struct {
		frames   int
		num, den uint64
		durNs    int64
	}{
		"23.976 fps": {32850, 24000, 1001, 41708333},
		"24 fps":     {30000, 24, 1, 41666666},
		"25 fps":     {30000, 25, 1, 40000000},
		"29.97 fps":  {30000, 30000, 1001, 33366666},
		"59.94 fps":  {60000, 60000, 1001, 16683333},
	} {
		t.Run(name, func(t *testing.T) {
			stts := roundedStts(tc.frames, tc.num, tc.den, 1000)
			if ns := headerVideoFrameDurNs(stts, 1000); ns != tc.durNs {
				t.Errorf("frame duration = %d ns, want %d", ns, tc.durNs)
			}
			if fps, want := headerFrameRate(stts, 1000), float64(tc.num)/float64(tc.den); fps != want {
				t.Errorf("frame rate = %.9f, want %.9f", fps, want)
			}
		})
	}

	// A rate that is not a standard one keeps the measured average.
	odd := roundedStts(30000, 47, 2, 1000) // 23.5 fps
	if ns := headerVideoFrameDurNs(odd, 1000); ns < 42553000 || ns > 42553400 {
		t.Errorf("23.5 fps: frame duration = %d ns, want the measured average (about 42553191)", ns)
	}
	// A short track cannot tell 24 from 23.976: no rate is asserted.
	if _, _, ok := standardFrameRate(6, 250, 1000); ok {
		t.Error("6 frames over 250 ms were pinned to one standard rate")
	}
}
