package mp4

import (
	"math"
	"testing"
)

// TestVariableFrameRateIsTheDominantRate pins that a variable-rate video reports the rate most of its frames play
// at, not its first frame's delta: a screen recording holding its first frame half a second read as 2 fps.
func TestVariableFrameRateIsTheDominantRate(t *testing.T) {
	var p bw
	p.u32(0) // version/flags
	p.u32(3) // entry_count
	p.u32(1)
	p.u32(300) // the first frame held 0.5 s at timescale 600
	p.u32(89)
	p.u32(20) // then 30 fps
	p.u32(10)
	p.u32(13) // then faster
	fps := headerFrameRate([]memBox{{typ: "stts", payload: p.b}}, 600)
	if math.Abs(fps-30) > 1e-9 {
		t.Errorf("fps = %v, want 30, the rate of 89 of its 100 frames", fps)
	}
}

// TestFrameRateOfDeltasPackedOneTickApart pins a table whose most frequent delta covers almost no time - reordered
// frames stored one tick apart in pairs (1, 5999 at 90 kHz) - reads at its average rate, not 90000 fps.
func TestFrameRateOfDeltasPackedOneTickApart(t *testing.T) {
	var p bw
	p.u32(0)
	p.u32(5)
	for _, e := range [][2]uint32{{13, 3000}, {136, 1}, {117, 5999}, {12, 6000}, {12, 2999}} {
		p.u32(e[0])
		p.u32(e[1])
	}
	fps := headerFrameRate([]memBox{{typ: "stts", payload: p.b}}, 90000)
	if want := 90000 * 290.0 / (13*3000 + 136 + 117*5999 + 12*6000 + 12*2999); math.Abs(fps-want) > 1e-9 {
		t.Errorf("fps = %v, want the average %v", fps, want)
	}
}
