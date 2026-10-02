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
