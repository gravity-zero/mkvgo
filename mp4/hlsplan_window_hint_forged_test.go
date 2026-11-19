package mp4

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// A window walk reads ahead no further than the file either: its range ran a look past the next window's cluster,
// past the end of a file whose last window is short.
func TestWindowWalkReadsAheadNoFurtherThanTheFile(t *testing.T) {
	data, err := os.ReadFile("../internal/testdata/sample_subs.mkv")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "in.mkv")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	type rng struct{ off, n int64 }
	var will []rng
	defer func(w func(*os.File, int64, int64)) { adviseWillNeed = w }(adviseWillNeed)
	adviseWillNeed = func(_ *os.File, off, n int64) { will = append(will, rng{off, n}) }
	defer func(f func(*os.File) bool) { networkSource = f }(networkSource)
	networkSource = func(*os.File) bool { return false }
	ctx := context.Background()
	plan, err := PlanHLS(ctx, path, Options{SegmentMs: 2000, VideoOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < plan.NumSegments(); n++ {
		h, err := plan.Open(ctx, plan.SegmentName(n))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.WriteTo(ctx, io.Discard); err != nil {
			t.Fatal(err)
		}
		h.Close()
	}
	if len(will) == 0 {
		t.Fatal("the walks read nothing ahead")
	}
	for _, w := range will {
		if w.off+w.n > int64(len(data)) {
			t.Errorf("read ahead [%d, %d) of a file of %d bytes", w.off, w.off+w.n, len(data))
		}
	}
}
