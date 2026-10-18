package mp4

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestEditOutputsNeverClaimWrongContentHashes: what an edit writes from a hashed MP4 certifies its own tracks, as a Matroska edit does.
func TestEditOutputsNeverClaimWrongContentHashes(t *testing.T) {
	ctx := context.Background()
	src := editFixture(t) // hashed: CONTENT_SHA256_1 and _2
	srt := filepath.Join(t.TempDir(), "s.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:02,000\nHi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := func(name string) string { return filepath.Join(t.TempDir(), name) }
	parts := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 3000}, {StartMs: 3000}}})
	cases := map[string]func(dst string) error{
		"rewrite":      func(dst string) error { return Rewrite(ctx, src, dst) },
		"remove-track": func(dst string) error { return RemoveTrack(ctx, src, dst, []uint64{1}) },
		"join":         func(dst string) error { return Join(ctx, parts, dst) },
		"mux reordered": func(dst string) error {
			return Mux(ctx, mkv.MuxOptions{OutputPath: dst, Tracks: []mkv.TrackInput{{SourcePath: src, TrackID: 2}, {SourcePath: src, TrackID: 1}}})
		},
		"add-track": func(dst string) error {
			return AddTrack(ctx, src, dst, mkv.TrackInput{SourcePath: realFixture("bframes_aac.mp4"), TrackID: 2})
		},
		"merge-subtitle": func(dst string) error { return MergeSubtitle(ctx, src, srt, dst, "fre", "") },
	}
	check := func(name, path string) {
		bad, err := VerifyContentHashes(ctx, path)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if len(bad) > 0 {
			t.Errorf("%s: %d tracks whose stored hash does not match their samples: %+v", name, len(bad), bad)
		}
	}
	for _, p := range parts {
		check(filepath.Base(p)+" of a split", p)
	}
	for name, run := range cases {
		dst := out(strings.ReplaceAll(name, " ", "_") + ".mp4")
		if err := run(dst); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		check(name, dst)
	}
}
