package mp4

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/internal/costguard"
	"github.com/gravity-zero/mkvgo/mkv"
)

// Planning an MP4's HLS costs in proportion to its samples and to its chapters (OpenMeta reads the moov lazily and is
// not timed here).
func TestMP4PlanCostGrowsLinearly(t *testing.T) {
	ctx := context.Background()
	built := map[[2]int]string{}
	build := func(t *testing.T, frames, chapters int) string {
		if p, ok := built[[2]int{frames, chapters}]; ok {
			return p
		}
		var blocks []genBlock
		for i := 0; i < frames; i++ {
			blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: []byte{0, 0, 0, 1, 0x65, byte(i)}})
		}
		var chs []mkv.Chapter
		for i := 0; i < chapters; i++ {
			chs = append(chs, mkv.Chapter{StartMs: int64(i) * int64(frames) * 40 / int64(chapters), Title: fmt.Sprintf("Chapter %d", i)})
		}
		src := buildMKVWithChapters(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240)}}, blocks, chs)
		dst := filepath.Join(t.TempDir(), "scale.mp4")
		if err := RemuxToMP4(ctx, src, dst, Options{FastStart: true}); err != nil {
			t.Fatal(err)
		}
		built[[2]int{frames, chapters}] = dst
		return dst
	}
	plan := func(path string) time.Duration {
		start := time.Now()
		if _, err := PlanHLS(ctx, path, Options{SegmentMs: 2000}); err != nil {
			t.Fatal(err)
		}
		return costguard.Since(start)
	}
	t.Run("samples", func(t *testing.T) {
		costguard.Linear(t, 150_000, func(n int) time.Duration { return plan(build(t, n, 1)) })
	})
	t.Run("chapters", func(t *testing.T) {
		costguard.Linear(t, 8_000, func(n int) time.Duration { return plan(build(t, 4*n, n)) })
	})
}
