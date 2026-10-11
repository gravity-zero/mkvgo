package mp4

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// A source replaced while its plan is being built never yields a plan that
// reads the new file at the old file's offsets: the build fails with
// ErrSourceChanged, or the plan serves the new file's own bytes.
func TestPlanBuiltAcrossAReplacement(t *testing.T) {
	for at := int32(1); at <= 6; at++ {
		t.Run(fmt.Sprintf("replaced at open %d", at), func(t *testing.T) { planAcrossReplacement(t, at) })
	}
}

func planAcrossReplacement(t *testing.T, at int32) {
	v1 := openGOPSource(t, 6, false, true)
	v2 := openGOPSource(t, 9, true, true)
	path := v1 + ".served"
	data, _ := os.ReadFile(v1)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	var renameErr error
	fs := &mkv.FS{
		Open: func(p string) (mkv.ReadSeekCloser, error) {
			f, err := os.Open(p)
			if err == nil && opens.Add(1) == at { // the build holds this handle: the file is replaced under it
				other, _ := os.ReadFile(v2)
				os.WriteFile(path+".tmp", other, 0o644)
				renameErr = os.Rename(path+".tmp", path)
			}
			return f, err
		},
		Stat: func(p string) (os.FileInfo, error) { return os.Stat(p) },
	}
	ctx := context.Background()
	plan, err := PlanHLS(ctx, path, Options{SegmentMs: openGOPMs, FS: fs, StreamFromFS: true})
	if renameErr != nil {
		t.Skipf("an open file cannot be replaced here (%v): no race to build across", renameErr)
	}
	if errors.Is(err, ErrSourceChanged) {
		return
	}
	if err != nil {
		t.Fatalf("PlanHLS: %v", err)
	}
	fresh, err := PlanHLS(ctx, v2, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range plan.Resources() {
		got, _, err := plan.Resource(ctx, name)
		if errors.Is(err, ErrSourceChanged) {
			continue
		}
		want, _, werr := fresh.Resource(ctx, name)
		if err != nil || werr != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: a plan built across the replacement served %d bytes (err %v) that are not the new file's (%d, err %v)", name, len(got), err, len(want), werr)
		}
	}
}
