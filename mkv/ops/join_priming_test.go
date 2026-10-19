package ops

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// discardPaddings lists the DiscardPadding of every block of track that states one, in ns.
func discardPaddings(t *testing.T, path string, track uint64) []int64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for {
		blk, err := br.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if blk.TrackNumber == track && blk.DiscardPaddingNs != 0 {
			out = append(out, blk.DiscardPaddingNs)
		}
	}
}

// TestJoin_DiscardsThePrimingLeftInsideAFrame: priming shorter than a frame (Opus pre-skip, AAC's last 64 samples)
// cannot be dropped as frames; it played at the seam and pushed the next file's sound late by that much.
func TestJoin_DiscardsThePrimingLeftInsideAFrame(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name         string
		delay, frame int64
		want         int64
		framesInJoin int
	}{
		{"opus", 6_500_000, 20_000_000, -6_500_000, 40},
		{"aac", 44_000_000, 21_333_333, -1_333_334, 38},
	} {
		audio := audioTrack(1)
		audio.CodecDelay = tc.delay
		audio.DefaultDurationNs = tc.frame
		var blocks []mkv.Block
		for i := int64(0); i < 20; i++ {
			blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: i * tc.frame / 1_000_000, Keyframe: true, Data: []byte{byte(i)}})
		}
		a := buildMinimalMKV(t, dir, tc.name+"-a.mkv", []mkv.Track{audio}, blocks, 427)
		b := buildMinimalMKV(t, dir, tc.name+"-b.mkv", []mkv.Track{audio}, blocks, 427)
		out := filepath.Join(dir, tc.name+".mkv")
		if err := Join(context.Background(), []string{a, b}, out); err != nil {
			t.Fatal(err)
		}
		if got := countBlocksFromFile(t, out, 1_000_000)[1]; got != tc.framesInJoin {
			t.Errorf("%s: %d frames, want %d", tc.name, got, tc.framesInJoin)
		}
		if got := discardPaddings(t, out, 1); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: DiscardPadding %v, want [%d] on the second file's first frame alone", tc.name, got, tc.want)
		}
	}
}
