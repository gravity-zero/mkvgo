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
			for _, p := range parts[1:] {
				if v := trackBlocks(collectAllBlocks(t, p))[matrix.VideoTrack]; v[0].Timecode != 0 || !v[0].Keyframe {
					t.Errorf("%s opens on %s, want its keyframe at 0", filepath.Base(p), blockSeq(v[:1]))
				}
			}
			for _, p := range parts[1 : 1+covered] {
				if subs := trackBlocks(collectAllBlocks(t, p))[matrix.SubtitleTrack]; len(subs) == 0 || subs[0].Timecode > 0 {
					t.Errorf("%s does not open on the cue on screen at its cut", filepath.Base(p))
				}
			}
			joined := filepath.Join(dir, "j.mkv")
			mustNil(t, Join(ctx, parts, joined))
			got := trackBlocks(collectAllBlocks(t, joined))
			for track, want := range joinedBlocks(c) {
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

// joinedBlocks is what a join of the case's parts gives back: the source, less, on an open GOP, the leading pictures
// stored after each cut keyframe; the hold the split gave the picture before them is dropped with its mark.
func joinedBlocks(c matrix.Case) map[uint64][]mkv.Block {
	out := c.Blocks()
	if !c.OpenGOP {
		return out
	}
	for _, cut := range c.ExpectedCuts() {
		var video []mkv.Block
		afterKey := false
		for _, b := range out[matrix.VideoTrack] {
			if b.Timecode == cut {
				afterKey = true
			} else if afterKey && b.Timecode < cut {
				continue
			} else if b.Timecode >= cut {
				afterKey = false
			}
			video = append(video, b)
		}
		out[matrix.VideoTrack] = video
	}
	return out
}

// TestSplitJoinSecondVideoMatrix: a second video track whose GOPs straddle the cuts comes back whole from split and join,
// each part opening it on one of its keyframes and lasting as its cut track does.
func TestSplitJoinSecondVideoMatrix(t *testing.T) {
	ctx := context.Background()
	for _, c := range matrix.SecondVideoCases() {
		t.Run(c.Name(), func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src.mkv")
			mustNil(t, c.BuildMKV(src))
			parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: c.Ranges()})
			mustNil(t, err)
			cuts := c.ExpectedCuts()
			if len(parts) != len(cuts)+1 {
				t.Fatalf("%d parts, want %d", len(parts), len(cuts)+1)
			}
			// The same case without the second track: what each part lasts must not move.
			twin := c
			twin.Second = false
			twinSrc := filepath.Join(dir, "twin.mkv")
			mustNil(t, twin.BuildMKV(twinSrc))
			twinParts, err := Split(ctx, mkv.SplitOptions{SourcePath: twinSrc, OutputDir: filepath.Join(dir, "twin"), Ranges: twin.Ranges()})
			mustNil(t, err)
			for k, p := range parts {
				blocks := trackBlocks(collectAllBlocks(t, p))
				if v := blocks[matrix.SecondVideo]; k > 0 && len(v) > 0 && !v[0].Keyframe {
					t.Errorf("%s opens the second video on %s, want one of its keyframes", filepath.Base(p), blockSeq(v[:1]))
				}
				pc, err := reader.Open(ctx, p)
				mustNil(t, err)
				tc, err := reader.Open(ctx, twinParts[k])
				mustNil(t, err)
				if d := pc.DurationMs - tc.DurationMs; d < -1 || d > 1 {
					t.Errorf("%s lasts %d ms, %d without the second video", filepath.Base(p), pc.DurationMs, tc.DurationMs)
				}
			}
			joined := filepath.Join(dir, "j.mkv")
			mustNil(t, Join(ctx, parts, joined))
			got := trackBlocks(collectAllBlocks(t, joined))
			want := joinedBlocks(c) // an open GOP's leading pictures stay out, as in every Matroska split
			for _, track := range []uint64{matrix.VideoTrack, matrix.SecondVideo, matrix.AudioTrack} {
				if g, w := blockSeq(got[track]), blockSeq(want[track]); g != w {
					t.Errorf("track %d: the join does not give the source back:\n got %s\nwant %s", track, g, w)
				}
			}
		})
	}
}
