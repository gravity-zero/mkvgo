package mp4

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/internal/costguard"
	"github.com/gravity-zero/mkvgo/mkv"
)

// subtitleScaleSource is n seconds of video, a keyframe every two, under a subtitle track of a cue a second, none
// stating a duration: every cue's end waits on the next one, which keeps a plan on its exact prefix path.
func subtitleScaleSource(t testing.TB, n int) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	var blocks []genBlock
	for i := 0; i < n; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 1000, key: i%2 == 0, data: []byte{0, 0, 0, 1, 0x65, byte(i)}})
		blocks = append(blocks, genBlock{track: 2, pts: int64(i)*1000 + 100, key: true, data: []byte(fmt.Sprintf("cue %d", i))})
	}
	sortGenBlocks(blocks)
	return buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt", Language: "eng"},
	}, blocks)
}

// Serving every WebVTT segment of a plan in order, as a player does, costs about four times as much for a track four
// times as long: each segment is not paid for with the whole track before it.
func TestHLSPlanSubtitleSegmentsScalesLinearly(t *testing.T) {
	ctx := context.Background()
	costguard.Linear(t, 8000, func(n int) time.Duration {
		plan, err := PlanHLS(ctx, subtitleScaleSource(t, n), Options{SegmentMs: 2000})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		for k := 0; k < plan.NumSegments(); k++ {
			if _, _, err := plan.Resource(ctx, fmt.Sprintf("sub1_%05d.vtt", k+1)); err != nil {
				t.Fatal(err)
			}
		}
		return costguard.Since(start)
	})
}

// Cues out of time order or long cost no more: one cue starting before the one before it, every other one doing so, or every cue stating a minute.
func TestHLSPlanSubtitleSegmentsOutOfOrderScaleLinearly(t *testing.T) {
	ctx := context.Background()
	for name, cue := range map[string]func(i int) mkv.Block{
		"one back": func(i int) mkv.Block {
			at := int64(i)*1000 + 100
			if i == 1 {
				at = 50 // in the first cluster, after cue 0, starting before it
			}
			return mkv.Block{Timecode: at, Data: []byte(fmt.Sprintf("cue %d", i))}
		},
		"every other back": decreasingPairs,
		"all long":         allLong,
	} {
		t.Run(name, func(t *testing.T) {
			costguard.Linear(t, 8000, func(n int) time.Duration {
				plan, err := PlanHLS(ctx, subIndexSource(t, n, n, cue), Options{SegmentMs: 2000})
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				for k := 0; k < plan.NumSegments(); k++ {
					if _, _, err := plan.Resource(ctx, fmt.Sprintf("sub1_%05d.vtt", k+1)); err != nil {
						t.Fatal(err)
					}
				}
				return costguard.Since(start)
			})
		})
	}
}

// The full pass writes every WebVTT segment in one go: a track four times as long costs about four times as much.
func TestRemuxToHLSSubtitleScalesLinearly(t *testing.T) {
	costguard.Linear(t, 4000, func(n int) time.Duration {
		src, dir := subtitleScaleSource(t, n), t.TempDir()
		start := time.Now()
		if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000}); err != nil {
			t.Fatal(err)
		}
		return costguard.Since(start)
	})
}
