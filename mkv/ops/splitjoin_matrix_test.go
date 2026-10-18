package ops

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/matrix"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestSplitJoinMatrix holds every matrix case to what split then join must keep: every block of every track, the
// chapters, the duration, the cue on screen at each cut, and no split link.
func TestSplitJoinMatrix(t *testing.T) {
	ctx := context.Background()
	for _, c := range matrix.Cases() {
		t.Run(c.Name(), func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src.mkv")
			mustNil(t, c.BuildMKV(src))
			parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: c.Ranges()})
			mustNil(t, err)
			if len(parts) != c.Cuts+1 {
				t.Fatalf("%d parts, want %d", len(parts), c.Cuts+1)
			}
			// The cue is on screen at the first cut, and at the second too when it runs over both.
			covered := map[matrix.Subtitle]int{matrix.CueOverCut: 1, matrix.CueOverTwoCuts: 2}[c.Subtitle]
			for _, p := range parts[1 : 1+covered] {
				if subs := trackBlocks(collectAllBlocks(t, p))[matrix.SubtitleTrack]; len(subs) == 0 || subs[0].Timecode > 0 {
					t.Errorf("%s does not open on the cue on screen at its cut", filepath.Base(p))
				}
			}
			joined := filepath.Join(dir, "j.mkv")
			mustNil(t, Join(ctx, parts, joined))
			got := trackBlocks(collectAllBlocks(t, joined))
			for track, want := range c.Blocks() {
				if g, w := blockSeq(got[track]), blockSeq(want); g != w {
					t.Errorf("track %d: the join does not give the source back:\n got %s\nwant %s", track, g, w)
				}
			}
			sc, err := reader.Open(ctx, src)
			mustNil(t, err)
			jc, err := reader.Open(ctx, joined)
			mustNil(t, err)
			if len(jc.Chapters) != len(sc.Chapters) {
				t.Fatalf("chapters %+v, want %+v", jc.Chapters, sc.Chapters)
			}
			for i, w := range sc.Chapters {
				if g := jc.Chapters[i]; g.Title != w.Title || g.StartMs != w.StartMs || g.EndMs != w.EndMs {
					t.Errorf("chapter %d: %+v, want %+v", i+1, g, w)
				}
			}
			if jc.Info.Duration != sc.Info.Duration {
				t.Errorf("duration %v, want %v", jc.Info.Duration, sc.Info.Duration)
			}
			if len(jc.Info.PrevUID) != 0 || len(jc.Info.NextUID) != 0 {
				t.Errorf("the join links to segments: prev %x next %x", jc.Info.PrevUID, jc.Info.NextUID)
			}
		})
	}
}
