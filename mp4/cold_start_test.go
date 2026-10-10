package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// coldFixture interleaves 20 ms audio with 40 ms video, keyframe every second, the audio written aheadMs ahead of the video.
func coldFixture(t testing.TB, aheadMs int64) string {
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	ai := 0
	for i := 0; i < 300; i++ {
		for ; int64(ai)*20 < int64(i)*40+aheadMs; ai++ {
			blocks = append(blocks, genBlock{track: 2, pts: int64(ai) * 20, key: true, data: []byte{0xAA, byte(ai)}})
		}
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: iframeTestFrame(i, i%25 == 0)})
	}
	return buildKeyframeClusteredMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}, blocks)
}

// A cold window looks one cued cluster back for blocks stored ahead of its
// own cluster, header-only, and further back when the head of the file showed
// a larger advance; whatever the advance, it serves the full pass's bytes.
func TestColdStartFollowsTheAudioAdvance(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		aheadMs int64
	}{{"audio in step with the video", 0}, {"audio 60 ms ahead", 60}, {"audio 300 ms ahead", 300}} {
		t.Run(tc.name, func(t *testing.T) {
			src := coldFixture(t, tc.aheadMs)
			dir, warm := assertPlanMatchesFullPass(t, src, 2000)
			cold, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
			if err != nil {
				t.Fatal(err)
			}
			learned := cold.aheadMs.Load()
			if (learned > 0) != (tc.aheadMs > 0) {
				t.Errorf("advance learned at the head: %d ms, fixture writes audio %d ms ahead", learned, tc.aheadMs)
			}
			for n := 1; n < cold.NumSegments(); n++ {
				if start := cold.coldStart(n); start >= cold.offsets[n] {
					t.Errorf("window %d: cold start %d does not precede the boundary cluster %d", n, start, cold.offsets[n])
				}
			}
			fts := warm.fts()
			for _, n := range []int{3, 1, 5} {
				name := renditionSegment(fts, 1, n)
				got, _, err := cold.Resource(ctx, name)
				if err != nil {
					t.Fatal(err)
				}
				want, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("cold %s differs from the full pass (%d vs %d bytes)", name, len(got), len(want))
				}
			}
		})
	}
}
