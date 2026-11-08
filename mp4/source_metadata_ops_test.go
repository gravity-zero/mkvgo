package mp4

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"
)

// A chmod, a chown or a hard link of the source is not a changed source: the bytes a plan's offsets point at are
// the same, and a running serve goes on. 0.66.0 counted them as changes (the inode change time in the stamp) and
// every later segment failed until a restart; a player gave up after its retries. A change of the modification
// time is still refused.
func TestPlanServesThroughChmodChownAndHardLink(t *testing.T) {
	statEveryRequest(t)
	src := openGOPSource(t, 3, false, true)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	open := func(what string) {
		t.Helper()
		if _, err := plan.Open(ctx, "seg00002.m4s"); err != nil {
			t.Fatalf("Open after %s: %v, want the segment served", what, err)
		}
		if _, _, err := plan.Resource(ctx, "seg_a1_00002.m4s"); err != nil {
			t.Fatalf("Resource after %s: %v, want the segment served", what, err)
		}
	}
	open("nothing")
	if err := os.Chmod(src, 0o640); err != nil {
		t.Fatal(err)
	}
	open("chmod")
	if runtime.GOOS != "windows" { // chown is not a Windows operation
		if err := os.Chown(src, os.Getuid(), os.Getgid()); err != nil {
			t.Fatal(err)
		}
		open("chown")
	}
	if err := os.Link(src, src+".link"); err != nil {
		t.Fatal(err)
	}
	open("a hard link")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(src, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Open(ctx, "seg00002.m4s"); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("Open after a touch: err = %v, want ErrSourceChanged", err)
	}
}
