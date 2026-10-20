package mp4

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// An MP4 text track whose cues start ever earlier is windowed in one pass, each cue ended as SettleCueEnds ends it.
func TestMP4SubWindowIsLinear(t *testing.T) {
	const n = 200_000
	tb := &mp4SubTable{samples: make([]fragSample, n), offs: make([]int64, n), startMs: make([]int64, n), durMs: make([]int64, n),
		state: make([]uint8, n), text: make([]string, n)}
	want := make([]subtitle.Cue, n)
	for i := range n {
		tb.startMs[i], tb.state[i], tb.text[i] = int64(n-i), subText, "a"
		want[i] = subtitle.Cue{StartMs: int64(n - i), Text: "a"}
	}
	subtitle.SettleCueEnds(want, 0)
	start := time.Now()
	got, err := tb.window(context.Background(), nil, math.MinInt64, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("window of %d cues took %v", n, d)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Error("the window ends its cues otherwise than SettleCueEnds")
	}
}
