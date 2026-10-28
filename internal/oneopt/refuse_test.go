package oneopt_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/httpfs"
	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/ops"
	"github.com/gravity-zero/mkvgo/mp4"
)

// TestASecondOptionsIsRefused: a second Options value was ignored, so a caller who split its settings over two got
// the defaults for the second half without a word. It is refused, before anything is read.
func TestASecondOptionsIsRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "missing.mkv"), filepath.Join(dir, "out.mkv")
	for name, err := range map[string]error{
		"ops.Reindex":       ops.Reindex(ctx, src, dst, mkv.Options{}, mkv.Options{DeepVerify: true}),
		"matroska.Validate": func() error { _, err := matroska.Validate(ctx, src, mkv.Options{}, mkv.Options{}); return err }(),
		"mp4.PlanHLS":       func() error { _, err := mp4.PlanHLS(ctx, src, mp4.Options{}, mp4.Options{SegmentMs: 2000}); return err }(),
	} {
		if !errors.Is(err, mkv.ErrTooManyOptions) {
			t.Errorf("%s with two Options: %v, want ErrTooManyOptions", name, err)
		}
	}
	defer func() {
		if r := recover(); r == nil {
			t.Error("httpfs.New with two Options did not refuse them")
		}
	}()
	httpfs.New(httpfs.Options{}, httpfs.Options{})
}
