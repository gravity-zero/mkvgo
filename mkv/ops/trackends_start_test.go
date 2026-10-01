package ops

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/livefixture"
)

// TestTrackEndsTimelineStart: a file that declares no duration still has one -
// the latest end minus where its timeline starts. The start is the first
// cluster's timestamp, and stays 0 (omitted) on a file that starts at 0.
func TestTrackEndsTimelineStart(t *testing.T) {
	ctx := context.Background()
	for name, o := range map[string]livefixture.Options{
		"clean":     {},
		"head junk": {JunkHead: 134, ShortUnknown: true},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "live.mkv")
			writeAll(t, path, livefixture.Build(o))
			report, err := TrackEnds(ctx, path)
			if err != nil {
				t.Fatalf("TrackEnds: %v", err)
			}
			if report.StartMs != livefixture.FirstTimestampMs {
				t.Fatalf("StartMs = %d, want %d", report.StartMs, livefixture.FirstTimestampMs)
			}
			if span := report.VideoEndMs - report.StartMs; span != livefixture.LastBlockMs-livefixture.FirstTimestampMs {
				t.Errorf("picture span = %d ms, want %d (last block - first timestamp)", span, livefixture.LastBlockMs-livefixture.FirstTimestampMs)
			}
		})
	}

	report, err := TrackEnds(ctx, sampleMKV)
	if err != nil {
		t.Fatalf("TrackEnds: %v", err)
	}
	if report.StartMs != 0 {
		t.Errorf("StartMs = %d on a file that starts at 0, want 0", report.StartMs)
	}
}

// TestAnalyzeRatesOverTheContentSpan: bitrates and frame rates are measured
// over the time the content spans. Divided by its end POSITION instead, a
// recording whose timeline starts at 12.345 s read as 20.3 fps where the
// stream is 24.
func TestAnalyzeRatesOverTheContentSpan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.mkv")
	writeAll(t, path, livefixture.Build(livefixture.Options{}))
	report, err := Analyze(context.Background(), path)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if report.StartMs != livefixture.FirstTimestampMs || report.DurationMs != livefixture.LastBlockMs {
		t.Fatalf("StartMs = %d, DurationMs = %d; want %d and %d (the end position)",
			report.StartMs, report.DurationMs, livefixture.FirstTimestampMs, livefixture.LastBlockMs)
	}
	frames := float64(livefixture.Clusters * livefixture.BlocksPerTrack)
	span := float64(livefixture.LastBlockMs - livefixture.FirstTimestampMs)
	if got, want := report.Tracks[0].FrameRateAvg, frames*1000/span; got != want {
		t.Errorf("video frame rate = %.3f, want %.3f (frames over the span, not over the end position: %.3f)",
			got, want, frames*1000/float64(livefixture.LastBlockMs))
	}
}
