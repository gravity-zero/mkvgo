package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestOutputCloseErrorSurfaced verifies the output ops surface a Close error on
// the success path (a custom FS that commits on Close) instead of dropping it,
// matching the Mux behaviour. Uses the closeErrWSC helper from coverage_test.go.
func TestOutputCloseErrorSurfaced(t *testing.T) {
	dir := t.TempDir()
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1), audioTrack(2)}, testBlocks(1), 300)
	subSrc := filepath.Join(dir, "s.srt")
	if err := os.WriteFile(subSrc, []byte("1\n00:00:00,000 --> 00:00:01,000\nhi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	failClose := &mkv.FS{
		Create: func(p string) (mkv.WriteSeekCloser, error) {
			f, err := os.Create(p)
			if err != nil {
				return nil, err
			}
			return &closeErrWSC{WriteSeekCloser: f}, nil
		},
		Open: func(p string) (mkv.ReadSeekCloser, error) { return os.Open(p) },
	}
	ctx := context.Background()
	out := func(name string) string { return filepath.Join(dir, name) }

	cases := []struct {
		name string
		run  func() error
	}{
		{"EditMetadata", func() error {
			return EditMetadata(ctx, src, out("a.mkv"), func(*mkv.Container) {}, mkv.Options{FS: failClose})
		}},
		{"RemoveTrack", func() error {
			return RemoveTrack(ctx, src, out("b.mkv"), []uint64{2}, mkv.Options{FS: failClose})
		}},
		{"Reindex", func() error { return Reindex(ctx, src, out("c.mkv"), mkv.Options{FS: failClose}) }},
		{"MergeSubtitle", func() error {
			return MergeSubtitle(ctx, src, subSrc, out("d.mkv"), "eng", "English", mkv.Options{FS: failClose})
		}},
	}
	for _, tc := range cases {
		if err := tc.run(); err == nil || !strings.Contains(err.Error(), "simulated close failure") {
			t.Errorf("%s: want the simulated close failure surfaced, got %v", tc.name, err)
		}
	}
}
