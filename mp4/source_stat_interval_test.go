package mp4

import (
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

// countingOSFS is the OS behind an FS port that counts the path stats.
func countingOSFS(stats *atomic.Int64) *mkv.FS {
	return &mkv.FS{
		Open: func(p string) (mkv.ReadSeekCloser, error) { return os.Open(p) },
		Stat: func(p string) (os.FileInfo, error) { stats.Add(1); return os.Stat(p) },
	}
}

// A local source is stat'ed by path once per interval, not per segment: every
// read is fstat'ed anyway.
func TestSourcePathStatOncePerInterval(t *testing.T) {
	src := openGOPSource(t, 6, false, true)
	var stats atomic.Int64
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs, FS: countingOSFS(&stats), StreamFromFS: true})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.pinned || !plan.osStat {
		t.Fatalf("an OS file behind an FS port must verify its reads (pinned %v, osStat %v)", plan.pinned, plan.osStat)
	}
	before := stats.Load()
	for i := 0; i < 20; i++ {
		h, err := plan.Open(ctx, "seg00002.m4s")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.WriteTo(ctx, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if n := stats.Load() - before; n != 1 {
		t.Fatalf("20 segments within a second made %d path stats, want 1", n)
	}
}

// Within the interval a changed source is caught by the read's fstat: the
// write fails before a byte of the source is copied, and the plan refuses
// every request after it, before any read.
func TestChangedSourceCaughtByFstatWithinInterval(t *testing.T) {
	copyFromSource(t)
	src := openGOPSource(t, 6, false, true)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	h, err := plan.Open(ctx, "seg00002.m4s") // stats the path, builds the table
	if err != nil {
		t.Fatal(err)
	}
	head := int64(len(h.table.heads[h.ti]))
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(src, later, later); err != nil {
		t.Fatal(err)
	}
	h, err = plan.Open(ctx, "seg00002.m4s") // within the interval: no path stat, the table is cached
	if err != nil {
		t.Fatalf("Open within the interval: %v", err)
	}
	m, err := h.WriteTo(ctx, io.Discard)
	if !errors.Is(err, ErrSourceChanged) || m > head {
		t.Fatalf("WriteTo over the changed source: %d bytes (head %d), err %v", m, head, err)
	}
	if _, err := plan.Open(ctx, "seg00003.m4s"); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("Open after the change was seen: err = %v, want ErrSourceChanged", err)
	}
	if _, _, err := plan.Resource(ctx, "seg_a1_00004.m4s"); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("Resource after the change was seen: err = %v, want ErrSourceChanged", err)
	}
}

// Once the interval has passed, the path stat fails the request before it
// writes anything: Open itself refuses.
func TestChangedSourceRefusedByOpenAfterInterval(t *testing.T) {
	old := sourceStatEvery.Load()
	sourceStatEvery.Store(int64(30 * time.Millisecond))
	t.Cleanup(func() { sourceStatEvery.Store(old) })
	src := openGOPSource(t, 6, false, true)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Open(ctx, "seg00002.m4s"); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(src, later, later); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := plan.Open(ctx, "seg00002.m4s"); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("Open after the interval: err = %v, want ErrSourceChanged", err)
	}
}
