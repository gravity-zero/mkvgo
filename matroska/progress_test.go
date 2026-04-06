package matroska

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func TestProgressCalledOnRemoveTrack(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()

	var calls atomic.Int64
	progress := func(processed, total int64) {
		calls.Add(1)
		if total <= 0 {
			t.Error("total should be positive")
		}
		if processed < 0 {
			t.Error("processed should be non-negative")
		}
	}

	assertNoErr(t, RemoveTrack(context.Background(), fixturePath, dir+"/out.mkv", []uint64{2}, Options{Progress: progress}))

	if calls.Load() == 0 {
		t.Error("progress was never called")
	}
	t.Logf("progress called %d times", calls.Load())
}

func TestProgressCalledOnEditMetadata(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()

	var calls atomic.Int64
	progress := func(processed, total int64) {
		calls.Add(1)
	}

	assertNoErr(t, EditMetadata(context.Background(), fixturePath, dir+"/out.mkv", func(c *Container) {
		c.Info.Title = "Progress Test"
	}, Options{Progress: progress}))

	if calls.Load() == 0 {
		t.Error("progress was never called")
	}
	t.Logf("progress called %d times", calls.Load())
}

func TestProgressFromNil(t *testing.T) {
	// No options = no crash
	fn := mkv.ProgressFrom(nil)
	if fn != nil {
		t.Error("expected nil")
	}

	fn = mkv.ProgressFrom([]Options{{}})
	if fn != nil {
		t.Error("expected nil for empty Options")
	}
}
