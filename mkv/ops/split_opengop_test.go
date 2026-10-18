package ops

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/matrix"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestSplitOpenGOPPartsOpenOnTheirKeyframe pins a cut on an open-GOP keyframe: the next part opens on it at 0 without
// the leading pictures stored after it (they decode only with the GOP before), the part before takes the sound stored
// among them and holds its last picture to the cut, so no sound is lost and a join keeps the timeline.
func TestSplitOpenGOPPartsOpenOnTheirKeyframe(t *testing.T) {
	c := matrix.Case{OpenGOP: true, Audio: true, Cuts: 1}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	mustNil(t, c.BuildMKV(src))
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: c.Ranges()})
	mustNil(t, err)
	cut := c.ExpectedCuts()[0]
	p1, p2 := trackBlocks(collectAllBlocks(t, parts[0])), trackBlocks(collectAllBlocks(t, parts[1]))
	if v := p2[matrix.VideoTrack]; v[0].Timecode != 0 || !v[0].Keyframe || v[1].Timecode <= 0 {
		t.Errorf("part 2 opens on %s, want its keyframe at 0 and no picture before it", blockSeq(v[:2]))
	}
	if n := len(p1[matrix.AudioTrack]) + len(p2[matrix.AudioTrack]); n != len(c.Blocks()[matrix.AudioTrack]) {
		t.Errorf("the parts hold %d audio frames, the source %d", n, len(c.Blocks()[matrix.AudioTrack]))
	}
	last := p1[matrix.VideoTrack][0]
	for _, b := range p1[matrix.VideoTrack] {
		if b.Timecode > last.Timecode {
			last = b
		}
	}
	if last.Timecode+last.Duration != cut {
		t.Errorf("part 1's last picture %d+%d ms ends short of the cut at %d", last.Timecode, last.Duration, cut)
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
