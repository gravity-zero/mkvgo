package ops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestMuxMergeErrorsHideTheQuery pins that a presigned source URL's signature stays out of mux, merge and compare errors.
func TestMuxMergeErrorsHideTheQuery(t *testing.T) {
	const src = "https://nas.example/movie.mkv?X-Amz-Signature=SECRET"
	fs := &mkv.FS{Open: func(string) (mkv.ReadSeekCloser, error) { return nil, errors.New("unreachable") }}
	opts := mkv.Options{FS: fs}
	errs := map[string]error{
		"mux":   Mux(context.Background(), mkv.MuxOptions{OutputPath: t.TempDir() + "/o.mkv", Tracks: []mkv.TrackInput{{SourcePath: src, TrackID: 1}}}, opts),
		"merge": Merge(context.Background(), mkv.MergeOptions{OutputPath: t.TempDir() + "/o.mkv", Inputs: []mkv.MergeInput{{SourcePath: src}}}, opts),
	}
	_, errs["compare"] = Compare(context.Background(), src, src, opts)
	_, errs["compare-blocks"] = CompareBlocks(context.Background(), src, src, opts)
	for op, err := range errs {
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: err = %v, want an error without the query", op, err)
		}
	}
}
