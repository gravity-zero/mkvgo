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

// TestTrackEndsOfAudioStatingNoFrameDuration: an audio track with neither a
// DefaultDuration nor BlockDurations says nothing about where its last block
// ends. Taken at its START, a laced last block put the audio short of the
// picture by as many frames as it held - a false "audio stops before the
// picture". The end is measured from the stride of the track's own blocks.
func TestTrackEndsOfAudioStatingNoFrameDuration(t *testing.T) {
	for name, o := range map[string]livefixture.Options{
		"one frame per block":         {},
		"two frames per block":        {LacedAudio: true},
		"two frames, in block groups": {LacedAudio: true, BlockGroups: true},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "live.mkv")
			writeAll(t, path, livefixture.Build(o))
			report, err := TrackEnds(context.Background(), path)
			if err != nil {
				t.Fatalf("TrackEnds: %v", err)
			}
			// Audio blocks are 250 ms apart: the last one ends a stride later.
			if got, want := report.Ends[1].EndMs, int64(livefixture.LastBlockMs+250); got != want {
				t.Errorf("audio ends at %d ms, want %d (the last block's start plus its frames)", got, want)
			}
			if report.AudioShortfallMs != 0 {
				t.Errorf("audio reported %d ms short of the picture", report.AudioShortfallMs)
			}
		})
	}
}

// TestTrackStrideMedian: the estimate takes the median of the recent strides -
// a gap right before the last block, or a run of short frames, moves it not.
func TestTrackStrideMedian(t *testing.T) {
	for name, tc := range map[string]struct {
		blocks []int64 // block timecodes, 8 frames each
		want   int64   // end of the last block
	}{
		"steady":                 {[]int64{0, 168, 336, 504, 672}, 672 + 168},
		"gap before the end":     {[]int64{0, 168, 336, 504, 2000}, 2000 + 168},
		"short frames once":      {[]int64{0, 168, 336, 357, 525, 693}, 693 + 168},
		"a single block, no way": {[]int64{40}, 40},
	} {
		t.Run(name, func(t *testing.T) {
			var s trackStride
			for _, tc := range tc.blocks {
				for f := 0; f < 8; f++ {
					s.add(tc)
				}
			}
			if got := s.endMs(); got != tc.want {
				t.Errorf("end = %d ms, want %d", got, tc.want)
			}
		})
	}
}

// TestTrackEndsPastDamage: the tail walk used to stop at the first element it
// could not read and report what it had seen - on a file damaged one second
// in, both tracks "ended" at one second. It walks past the damage, reports
// the real ends, and says how many bytes it had to pass over.
func TestTrackEndsPastDamage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.mkv")
	writeAll(t, path, livefixture.Build(livefixture.Options{Overrun: true}))
	report, err := TrackEnds(context.Background(), path)
	if err != nil {
		t.Fatalf("TrackEnds: %v", err)
	}
	if report.VideoEndMs != livefixture.LastBlockMs {
		t.Errorf("picture ends at %d ms, want %d (the last block, past the damage)", report.VideoEndMs, livefixture.LastBlockMs)
	}
	if report.SkippedBytes == 0 {
		t.Error("the damage the walk passed over is not reported")
	}

	writeAll(t, path, livefixture.Build(livefixture.Options{}))
	if report, err = TrackEnds(context.Background(), path); err != nil || report.SkippedBytes != 0 {
		t.Errorf("a sound file: %d skipped bytes (err %v), want none", report.SkippedBytes, err)
	}
}
