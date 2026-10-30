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
	"github.com/gravity-zero/mkvgo/mkvhttp"
	"github.com/gravity-zero/mkvgo/mp4"
)

// TestASecondOptionsIsRefused: a second Options value was ignored, so a caller who split its settings over two got
// the defaults for the second half without a word. It is refused, before anything is read; a constructor that
// returns no error panics where it is built, and its Checked variant returns the refusal.
func TestASecondOptionsIsRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "missing.mkv"), filepath.Join(dir, "out.mkv")
	for name, err := range map[string]error{
		"ops.Reindex":            ops.Reindex(ctx, src, dst, mkv.Options{}, mkv.Options{DeepVerify: true}),
		"matroska.Validate":      func() error { _, err := matroska.Validate(ctx, src, mkv.Options{}, mkv.Options{}); return err }(),
		"mp4.PlanHLS":            func() error { _, err := mp4.PlanHLS(ctx, src, mp4.Options{}, mp4.Options{SegmentMs: 2000}); return err }(),
		"httpfs.NewChecked":      func() error { _, err := httpfs.NewChecked(httpfs.Options{}, httpfs.Options{}); return err }(),
		"httpfs.HybridChecked":   func() error { _, err := httpfs.HybridChecked(httpfs.Options{}, httpfs.Options{}); return err }(),
		"mkvhttp.HandlerChecked": func() error { _, err := mkvhttp.HandlerChecked(nil, mkvhttp.Options{}, mkvhttp.Options{}); return err }(),
		"mkvhttp.FileHandlerChecked": func() error {
			_, err := mkvhttp.FileHandlerChecked(src, mkvhttp.Options{}, mkvhttp.Options{})
			return err
		}(),
	} {
		if !errors.Is(err, mkv.ErrTooManyOptions) {
			t.Errorf("%s with two Options: %v, want ErrTooManyOptions", name, err)
		}
	}
	for name, build := range map[string]func(){
		"httpfs.New":          func() { httpfs.New(httpfs.Options{}, httpfs.Options{}) },
		"httpfs.Hybrid":       func() { httpfs.Hybrid(httpfs.Options{}, httpfs.Options{}) },
		"mkvhttp.Handler":     func() { mkvhttp.Handler(nil, mkvhttp.Options{}, mkvhttp.Options{}) },
		"mkvhttp.FileHandler": func() { mkvhttp.FileHandler(src, mkvhttp.Options{}, mkvhttp.Options{}) },
	} {
		func() {
			defer func() {
				if r, _ := recover().(error); !errors.Is(r, mkv.ErrTooManyOptions) {
					t.Errorf("%s with two Options: built without panicking on ErrTooManyOptions (recovered %v)", name, r)
				}
			}()
			build()
		}()
	}
	for name, err := range map[string]error{
		"httpfs.NewChecked":          func() error { _, err := httpfs.NewChecked(httpfs.Options{}); return err }(),
		"httpfs.HybridChecked":       func() error { _, err := httpfs.HybridChecked(); return err }(),
		"mkvhttp.HandlerChecked":     func() error { _, err := mkvhttp.HandlerChecked(nil, mkvhttp.Options{}); return err }(),
		"mkvhttp.FileHandlerChecked": func() error { _, err := mkvhttp.FileHandlerChecked(src); return err }(),
	} {
		if err != nil {
			t.Errorf("%s with one Options or none: %v", name, err)
		}
	}
}
