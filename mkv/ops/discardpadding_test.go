package ops

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestSplitJoinKeepDiscardPadding pins that the padding a stream's last audio frame tells a decoder to drop survives
// a split and its join, as every other byte of the block does.
func TestSplitJoinKeepDiscardPadding(t *testing.T) {
	dir := t.TempDir()
	var blocks []mkv.Block
	for tc := int64(0); tc < 4000; tc += 20 {
		if tc%40 == 0 {
			blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: tc%1000 == 0, Data: []byte{0, 0, 0, 1, 0x65}})
		}
		blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")})
	}
	blocks[len(blocks)-1].DiscardPaddingNs = 6_500_000
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1), audioTrack(2)}, blocks, 4000)
	if got := lastAudio(t, src).DiscardPaddingNs; got != 6_500_000 {
		t.Fatalf("source written with padding %d ns, want 6500000", got)
	}
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: []mkv.TimeRange{{EndMs: 2000}, {StartMs: 2000}}})
	mustNil(t, err)
	joined := filepath.Join(dir, "j.mkv")
	mustNil(t, Join(context.Background(), parts, joined))
	for _, p := range []string{parts[1], joined} {
		if got := lastAudio(t, p).DiscardPaddingNs; got != 6_500_000 {
			t.Errorf("%s: last audio padding %d ns, want 6500000", filepath.Base(p), got)
		}
	}
}

func lastAudio(t *testing.T, path string) mkv.Block {
	audio := trackBlocks(collectAllBlocks(t, path))[2]
	return audio[len(audio)-1]
}
