package mp4

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/costguard"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// An MP4 text track whose cues start ever earlier is windowed in one pass, each cue ended as SettleCueEnds ends it:
// counted in samples visited and heap allocated, so the verdict does not hang on the machine's load.
func TestMP4SubWindowCostGrowsLinearly(t *testing.T) {
	costguard.LinearOps(t, 20_000, func(n int) uint64 {
		tb := &mp4SubTable{samples: make([]fragSample, n), offs: make([]int64, n), startMs: make([]int64, n), durMs: make([]int64, n),
			state: make([]uint8, n), text: make([]string, n)}
		want := make([]subtitle.Cue, n)
		for i := range n {
			tb.startMs[i], tb.state[i], tb.text[i] = int64(n-i), subText, "a"
			want[i] = subtitle.Cue{StartMs: int64(n - i), Text: "a"}
		}
		subtitle.SettleCueEnds(want, 0)
		var got []subtitle.Cue
		var err error
		heap := costguard.HeapWork(func() { got, err = tb.window(context.Background(), nil, math.MinInt64, math.MaxInt64) })
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatal("the window ends its cues otherwise than SettleCueEnds")
		}
		return uint64(tb.steps) + heap
	})
}
