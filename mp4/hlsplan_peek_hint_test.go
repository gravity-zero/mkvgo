package mp4

import (
	"context"
	"math"
	"os"
	"sync"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

type hintRecorder struct {
	*os.File
	mu    *sync.Mutex
	hints *[][2]int64
}

// HintRange records a hint; the plan's peeks run concurrently.
func (h hintRecorder) HintRange(off, end int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.hints = append(*h.hints, [2]int64{off, end})
}

// A remote source is asked for no range past the file's end, and never for an open-ended one: the plan's head peek
// hinted its source directly, past the cap the window walks have, and to MaxInt64 on a single-window file (an
// open-ended request a remote source then dropped, the connection with it).
func TestPlanHintsNoRangePastTheFile(t *testing.T) {
	src := buildSeekFixture(t)
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	var hints [][2]int64
	var mu sync.Mutex
	fs := &mkv.FS{Open: func(p string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		return hintRecorder{File: f, mu: &mu, hints: &hints}, nil
	}}
	ctx := context.Background()
	for _, segMs := range []int64{2000, 10_000_000} { // many windows, then one
		mu.Lock()
		hints = nil
		mu.Unlock()
		plan, err := PlanHLS(ctx, src, Options{SegmentMs: segMs, FS: fs, StreamFromFS: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := plan.Segment(ctx, 0); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		got := append([][2]int64(nil), hints...)
		mu.Unlock()
		if len(got) == 0 {
			t.Fatal("no range hinted")
		}
		for _, h := range got {
			if h[1] == math.MaxInt64 || h[1] > st.Size() {
				t.Errorf("SegmentMs %d: range [%d, %d) hinted for a file of %d bytes", segMs, h[0], h[1], st.Size())
			}
		}
	}
}
