package ops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mp4"
)

// TestAnalyze_MP4MatchesItsMatroskaRemux: an MP4 is analyzed from its sample
// tables, whatever its name, and reads as the Matroska remux of it does.
func TestAnalyze_MP4MatchesItsMatroskaRemux(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"bframes_aac.mp4", "fps30.mp4", "hevc_opengop.mp4", "negative_cts.mp4", "interleaved.mp4"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "internal", "testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(t.TempDir(), "mislabeled.mkv")
			if err := os.WriteFile(src, data, 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := Analyze(ctx, src)
			if err != nil {
				t.Fatalf("Analyze(MP4 named .mkv): %v", err)
			}
			remux := filepath.Join(t.TempDir(), "remux.mkv")
			if err := mp4.RemuxFromMP4(ctx, src, remux); err != nil {
				t.Fatal(err)
			}
			want, err := Analyze(ctx, remux)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Tracks) != len(want.Tracks) || len(got.Tracks) == 0 {
				t.Fatalf("%d tracks, the remux %d", len(got.Tracks), len(want.Tracks))
			}
			for i, g := range got.Tracks {
				w := want.Tracks[i]
				if g.Frames != w.Frames || g.Keyframes != w.Keyframes || g.Bytes != w.Bytes ||
					g.FrameRateMode != w.FrameRateMode || g.MinGopFrames != w.MinGopFrames || g.MaxGopFrames != w.MaxGopFrames {
					t.Errorf("track %d: MP4 %+v\nremux %+v", g.TrackID, g, w)
				}
				if d := g.DurationMs - w.DurationMs; d < -50 || d > 50 {
					t.Errorf("track %d: duration %d ms, the remux %d", g.TrackID, g.DurationMs, w.DurationMs)
				}
			}
			if got.ClusterCount != 0 || got.BlockCount == 0 {
				t.Errorf("ClusterCount %d, BlockCount %d: want 0 and the sample count", got.ClusterCount, got.BlockCount)
			}
		})
	}
}
