package mp4

import (
	"context"
	"io"
	"os"
	"testing"
)

// A segment's read-ahead covers its whole window, every rendition's samples, not the segment's own span alone: a
// player fetches a window's audio and video together, and the first request read ahead from its own first sample
// only, so the window came off the disk in two runs with a short one between (measured on a fragmented MP4, whose
// audio runs follow each fragment's video: 1,656 disk reads of 681 KiB for a file a sequential read takes in 1,104
// of 1 MiB, the audio asked for first).
func TestPrefetchCoversTheWholeWindow(t *testing.T) {
	type rng struct{ off, n int64 }
	var will []rng
	defer func(w func(*os.File, int64, int64)) { adviseWillNeed = w }(adviseWillNeed)
	adviseWillNeed = func(_ *os.File, off, n int64) { will = append(will, rng{off, n}) }
	defer func(f func(*os.File) bool) { networkSource = f }(networkSource)
	networkSource = func(*os.File) bool { return false }
	ctx := context.Background()
	plan, err := PlanHLS(ctx, interleavedMP4, Options{SegmentMs: 2000, StreamFromFS: true})
	if err != nil {
		t.Fatal(err)
	}
	const n = 1
	lo, hi := int64(-1), int64(0)
	for ti := range plan.tracks {
		seg, offs := plan.mp4Segment(ti, n)
		if len(offs) == 0 {
			continue
		}
		if lo < 0 || offs[0] < lo {
			lo = offs[0]
		}
		if end := offs[len(offs)-1] + int64(seg.samples[len(seg.samples)-1].size); end > hi {
			hi = end
		}
	}
	for _, ti := range []int{1, 0} { // the audio first, as a player asks
		will = nil
		h, err := plan.Open(ctx, renditionSegment(plan.fts(), ti, n))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.WriteTo(ctx, io.Discard); err != nil {
			t.Fatal(err)
		}
		h.Close()
		if len(will) == 0 {
			t.Fatalf("rendition %d: nothing read ahead", ti)
		}
		first, last := will[0], will[len(will)-1]
		if first.off != lo || last.off+last.n < hi {
			t.Errorf("rendition %d: read ahead [%d, %d), want the window [%d, %d)", ti, first.off, last.off+last.n, lo, hi)
		}
	}
}
