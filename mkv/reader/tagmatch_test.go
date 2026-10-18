package reader

import (
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestTagTrackMatchingLinear: matching statistics tags to tracks walked every tag for every track: 50,000 of each,
// a few megabytes of file, cost billions of comparisons. Tags are looked up by their target UID.
func TestTagTrackMatchingLinear(t *testing.T) {
	c := &mkv.Container{}
	for i := 0; i < 50000; i++ {
		c.Tracks = append(c.Tracks, mkv.Track{ID: uint64(i + 1), UID: uint64(i + 1), Type: mkv.VideoTrack})
		c.Tags = append(c.Tags, mkv.Tag{TargetID: uint64(1 << 40), SimpleTags: []mkv.SimpleTag{{Name: "BPS", Value: "1"}}})
	}
	c.Tags[0].TargetID = 7
	start := time.Now()
	promoteTrackBitrate(c)
	statsFrameRates(c)
	if d := time.Since(start); d > time.Second {
		t.Errorf("50,000 tracks and tags matched in %v", d)
	}
	if b := c.Tracks[6].Bitrate; b == nil || *b != 1 {
		t.Errorf("track UID 7 bitrate %v, want 1", b)
	}
}
