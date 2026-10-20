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
