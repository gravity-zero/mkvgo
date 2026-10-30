package oneopt_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
// returns no error returns a value every use of which fails with the refusal, and does not panic.
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
	if _, err := httpfs.New(httpfs.Options{}, httpfs.Options{}).Port().DoStat("http://127.0.0.1:1/x"); !errors.Is(err, mkv.ErrTooManyOptions) {
		t.Errorf("httpfs.New with two Options: stat %v, want ErrTooManyOptions", err)
	}
	if _, err := httpfs.Hybrid(httpfs.Options{}, httpfs.Options{}).DoOpen(src); !errors.Is(err, mkv.ErrTooManyOptions) {
		t.Errorf("httpfs.Hybrid with two Options: open of a local path %v, want ErrTooManyOptions", err)
	}
	for name, h := range map[string]http.Handler{
		"mkvhttp.Handler":     mkvhttp.Handler(nil, mkvhttp.Options{}, mkvhttp.Options{}),
		"mkvhttp.FileHandler": mkvhttp.FileHandler(src, mkvhttp.Options{}, mkvhttp.Options{}),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), mkv.ErrTooManyOptions.Error()) {
			t.Errorf("%s with two Options: %d %q, want 500 naming the refusal", name, rec.Code, rec.Body.String())
		}
	}
}
