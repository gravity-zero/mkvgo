package ops

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/matrix"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestSplitKeepsOpenGOPLeadingPictures pins that a part opening on an open-GOP keyframe keeps the pictures stored
// after it and shown before it, ahead of its zero, as an MP4 edit list would: the part before stops at the keyframe.
func TestSplitKeepsOpenGOPLeadingPictures(t *testing.T) {
	c := matrix.Case{OpenGOP: true, Audio: true, Cuts: 1}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	mustNil(t, c.BuildMKV(src))
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: c.Ranges()})
	mustNil(t, err)
	cut := c.ExpectedCuts()[0]
	video := trackBlocks(collectAllBlocks(t, parts[1]))[matrix.VideoTrack]
	if len(video) < 3 || video[0].Timecode != 0 || !video[0].Keyframe || video[1].Timecode != -80 || video[2].Timecode != -40 {
		t.Fatalf("part 2 opens on %s, want the keyframe at 0 then its leading pictures at -80 and -40", blockSeq(video[:min(3, len(video))]))
	}
	audio := trackBlocks(collectAllBlocks(t, parts[0]))[matrix.AudioTrack]
	audio2 := trackBlocks(collectAllBlocks(t, parts[1]))[matrix.AudioTrack]
	if n := len(audio) + len(audio2); n != len(c.Blocks()[matrix.AudioTrack]) {
		t.Errorf("the parts hold %d audio frames, the source %d", n, len(c.Blocks()[matrix.AudioTrack]))
	}
	if last := audio[len(audio)-1].Timecode; last >= cut {
		t.Errorf("part 1 holds audio at %d ms, past its cut at %d", last, cut)
	}
}

// TestSplitOpensOnTheCueAtItsZero pins that a cue on screen across a cut opens the next part at its zero, even when
// sound muxed ahead of the keyframe puts that zero a few ms before the first picture.
func TestSplitOpensOnTheCueAtItsZero(t *testing.T) {
	c := matrix.Case{OpenGOP: true, Audio: true, NTSC: true, Subtitle: matrix.CueOverCut, Cuts: 1}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	mustNil(t, c.BuildMKV(src))
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: c.Ranges()})
	mustNil(t, err)
	cue := c.Blocks()[matrix.SubtitleTrack][0]
	b := trackBlocks(collectAllBlocks(t, parts[1]))
	video, subs := b[matrix.VideoTrack], b[matrix.SubtitleTrack]
	if len(subs) != 1 || subs[0].Timecode != 0 || subs[0].Timecode+subs[0].Duration != video[0].Timecode+cue.Timecode+cue.Duration-c.ExpectedCuts()[0] {
		t.Errorf("part 2 subtitles %s with its picture at %d ms, want the cue from 0 to where it ends", blockSeq(subs), video[0].Timecode)
	}
}

func trackBlocks(blocks []mkv.Block) map[uint64][]mkv.Block {
	out := map[uint64][]mkv.Block{}
	for _, b := range blocks {
		out[b.TrackNumber] = append(out[b.TrackNumber], b)
	}
	return out
}

func blockSeq(blocks []mkv.Block) string {
	s := ""
	for _, b := range blocks {
		s += fmt.Sprintf("%d/%v/%d/%x ", b.Timecode, b.Keyframe, b.Duration, b.Data)
	}
	return s
}
