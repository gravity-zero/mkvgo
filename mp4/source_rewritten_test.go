package mp4

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// A source rewritten in place at the same size, its modification time put back (cp -p of a file of the same size
// over it), is a changed source: the size and the mtime say nothing, the inode change time does. Measured on a
// union mount keeping the page cache: the plan went on serving, and the pages read before the rewrite were the
// old file's.
func TestPlanDetectsInPlaceRewriteWithSameSizeAndMtime(t *testing.T) {
	statEveryRequest(t)
	src := openGOPSource(t, 3, false, true)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Open(ctx, "seg00001.m4s"); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := changeTime(before); !ok {
		t.Skip("no inode change time on this system: the stamp is size and mtime alone")
	}
	f, err := os.OpenFile(src, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if _, err := f.ReadAt(buf, before.Size()/2); err != nil {
		t.Fatal(err)
	}
	for i := range buf {
		buf[i] ^= 0x5A
	}
	if _, err := f.WriteAt(buf, before.Size()/2); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// The kernel stamps ctime from a coarse clock (a few ms): within one tick of the plan's stat the rewrite would
	// not move it, which is not the case under test; setting the mtime back moves ctime again, until it differs.
	ct0, _ := changeTime(before)
	var after os.FileInfo
	for try := 0; try < 100; try++ {
		if err := os.Chtimes(src, before.ModTime(), before.ModTime()); err != nil {
			t.Fatal(err)
		}
		if after, err = os.Stat(src); err != nil {
			t.Fatal(err)
		}
		if ct, _ := changeTime(after); ct != ct0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ct, _ := changeTime(after); ct == ct0 {
		t.Fatal("the inode change time did not move after a write and a utimes")
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the rewrite changed the size or the mtime (%d/%v -> %d/%v): not the case under test", before.Size(), before.ModTime(), after.Size(), after.ModTime())
	}
	if _, err := plan.Open(ctx, "seg00001.m4s"); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("Open after an in-place rewrite at the same size and mtime: err = %v, want ErrSourceChanged", err)
	}
}
