package mp4

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestRemoveTrackShortensTheMovieToWhatIsLeft: the chapter track runs to the old end, but only media tracks set the new length.
func TestRemoveTrackShortensTheMovieToWhatIsLeft(t *testing.T) {
	var blocks []genBlock
	for i := range 150 { // video, 6 s
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := range 450 { // audio, 9 s
		blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(blocks)
	src := filepath.Join(t.TempDir(), "in.mp4")
	if err := RemuxToMP4(context.Background(), buildMKV(t, synthFixtureTracks(), blocks), src, Options{}); err != nil {
		t.Fatal(err)
	}
	if err := EditInPlace(context.Background(), src, func(c *mkv.Container) {
		c.Chapters = []mkv.Chapter{{ID: 1, Title: "A", StartMs: 0}, {ID: 2, Title: "B", StartMs: 3000}}
	}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "video.mp4")
	if err := RemoveTrack(context.Background(), src, dst, []uint64{2}); err != nil {
		t.Fatal(err)
	}
	if got := mustMeta(t, dst).DurationMs; got < 5900 || got > 6100 {
		t.Errorf("movie lasts %d ms after removing the 9 s audio, want the 6 s video", got)
	}
}
