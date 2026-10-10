package mp4

import "testing"

// A stride measured too long accumulates: the grid clock only ever steps
// forward, so a block whose timecode lands behind the running index takes the
// next slot and the error never unwinds. A stride measured too short cannot
// accumulate: every block re-anchors the clock forward on its own timecode and
// the error stays under one block.
func TestStrideErrorDirection(t *testing.T) {
	const blocks, frames, mts = 3000, 8, 48000
	build := func() []fragSample {
		var s []fragSample
		for b := 0; b < blocks; b++ {
			tc := (int64(b)*frames*1024*1000 + mts/2) / mts // the muxer's millisecond rounding of exact 1024-sample frames
			for f := 0; f < frames; f++ {
				s = append(s, fragSample{ptsMs: tc, blockPtsMs: tc, sync: true})
			}
		}
		return s
	}
	deviation := func(gridTS int64) float64 {
		s := build()
		fillFragTimingTS(s, nil, 0, mts, gridTS)
		var worst float64
		for i := 0; i < len(s); i += frames {
			if d := float64(s[i].dtsTS)*1000/mts - float64(s[i].blockPtsMs-s[0].blockPtsMs); d > worst || -d > worst {
				worst = d
				if worst < 0 {
					worst = -worst
				}
			}
		}
		return worst
	}
	frameMs := 1024.0 * 1000 / mts
	if d := deviation(1026); d < 20*frameMs {
		t.Errorf("stride too long (1026): deviation %.1f ms, expected an accumulation of many frames", d)
	}
	if d := deviation(1020); d > frameMs+1 {
		t.Errorf("stride too short (1020): deviation %.1f ms, expected under one frame", d)
	}
	if d := deviation(1024); d > 1 {
		t.Errorf("exact stride: deviation %.3f ms", d)
	}
}
