package mp4

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

const tmcdFixture = "../internal/testdata/tmcd.mov" // track 1 H.264, track 2 a timecode (tmcd) track, not media

// TestStrictRefusesNonMediaTracks: split, join and merge leave an MP4's non-media track out with its reason by
// default; Options.Strict refuses them by the track's name, as every other operation does.
func TestStrictRefusesNonMediaTracks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var drops []DroppedTrack
	collect := func(d DroppedTrack) { drops = append(drops, d) }
	if _, err := Split(ctx, mkv.SplitOptions{SourcePath: tmcdFixture, OutputDir: filepath.Join(dir, "parts"), Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 300}}}, Options{OnDrop: collect}); err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(drops) != 1 || drops[0].ID != 2 || drops[0].Codec != "tmcd" {
		t.Errorf("split: drops = %+v, want the tmcd track 2", drops)
	}
	strict := func(name string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "track 2 (tmcd)") || !strings.Contains(err.Error(), "Options.Strict") {
			t.Errorf("%s strict: err = %v, want track 2 (tmcd) refused by Options.Strict", name, err)
		}
	}
	_, err := Split(ctx, mkv.SplitOptions{SourcePath: tmcdFixture, OutputDir: filepath.Join(dir, "parts2"), Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 300}}}, Options{Strict: true})
	strict("split", err)
	strict("join", Join(ctx, []string{tmcdFixture, tmcdFixture}, filepath.Join(dir, "join.mp4"), Options{Strict: true}))
	strict("merge", Merge(ctx, mkv.MergeOptions{OutputPath: filepath.Join(dir, "merge.mp4"), Inputs: []mkv.MergeInput{{SourcePath: tmcdFixture}}}, Options{Strict: true}))
}
