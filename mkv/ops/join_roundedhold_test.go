package ops

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/matrix"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// The hold a split gives the last picture before a cut that falls between milliseconds is dropped only where its
// linked next part follows: the part keeps it, and a join of files that do not continue each other keeps it.
func TestJoinDropsTheRoundingHoldOnlyAtALinkedSeam(t *testing.T) {
	ctx := context.Background()
	c := matrix.Case{NTSC: true, Cuts: 1}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	mustNil(t, c.BuildMKV(src))
	parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: c.Ranges()})
	mustNil(t, err)
	held := func(path string) int {
		n := 0
		for _, b := range trackBlocks(collectAllBlocks(t, path))[matrix.VideoTrack] {
			if b.Duration > 0 {
				n++
			}
		}
		return n
	}
	if n := held(parts[0]); n != 1 {
		t.Fatalf("the part before the cut holds %d pictures, want its last one held to the cut", n)
	}
	linked, apart := filepath.Join(dir, "linked.mkv"), filepath.Join(dir, "apart.mkv")
	mustNil(t, Join(ctx, parts, linked))
	mustNil(t, Join(ctx, []string{parts[0], parts[0]}, apart))
	if n := held(linked); n != 0 {
		t.Errorf("the join of the linked parts holds %d pictures, the source none", n)
	}
	if n := held(apart); n != 2 {
		t.Errorf("a join of files that do not continue each other holds %d pictures, want each copy's last one", n)
	}
	// The part marks the picture it holds; the joins carry no mark.
	marks := func(path string) int {
		c, err := reader.Open(ctx, path)
		mustNil(t, err)
		n := 0
		for _, tag := range c.Tags {
			for _, st := range tag.SimpleTags {
				if st.Name == splitHoldTag {
					n++
				}
			}
		}
		return n
	}
	if n := marks(parts[0]); n != 1 {
		t.Errorf("the part before the cut carries %d hold marks, want 1", n)
	}
	for _, j := range []string{linked, apart} {
		if n := marks(j); n != 0 {
			t.Errorf("%s carries %d hold marks", filepath.Base(j), n)
		}
	}
}

// A source whose every picture states its duration gets them all back from split then join, the last before the cut
// included: only a hold the split added is dropped.
func TestJoinKeepsTheDurationsASourceStates(t *testing.T) {
	ctx := context.Background()
	c := matrix.Case{NTSC: true, Cuts: 1}
	dir := t.TempDir()
	video := c.Blocks()[matrix.VideoTrack]
	var sets [][]mkv.Block
	for j := range video {
		if j+1 < len(video) {
			video[j].Duration = video[j+1].Timecode - video[j].Timecode
		} else {
			video[j].Duration = 33
		}
		if video[j].Keyframe {
			sets = append(sets, nil)
		}
		sets[len(sets)-1] = append(sets[len(sets)-1], video[j])
	}
	src := buildMKVWithCuesDur(t, dir, "timed.mkv", c.Tracks(), sets, nil, c.EndMs())
	parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: c.Ranges()})
	mustNil(t, err)
	joined := filepath.Join(dir, "j.mkv")
	mustNil(t, Join(ctx, parts, joined))
	if g, w := blockSeq(trackBlocks(collectAllBlocks(t, joined))[matrix.VideoTrack]), blockSeq(video); g != w {
		t.Errorf("the join does not give the source back:\n got %s\nwant %s", g, w)
	}
}

// A source that states the durations of some pictures only, the first not, gets them back from split then join: only
// a hold the split added, on a part's last picture, is dropped.
func TestJoinKeepsTheDurationsASourceStatesOnSomePictures(t *testing.T) {
	ctx := context.Background()
	c := matrix.Case{NTSC: true, Cuts: 2}
	dir := t.TempDir()
	video := c.Blocks()[matrix.VideoTrack]
	var sets [][]mkv.Block
	for j := range video {
		if j%10 == 5 && j+1 < len(video) {
			video[j].Duration = video[j+1].Timecode - video[j].Timecode
		}
		if video[j].Keyframe {
			sets = append(sets, nil)
		}
		sets[len(sets)-1] = append(sets[len(sets)-1], video[j])
	}
	src := buildMKVWithCuesDur(t, dir, "sparse.mkv", c.Tracks(), sets, nil, c.EndMs())
	parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: c.Ranges()})
	mustNil(t, err)
	joined := filepath.Join(dir, "j.mkv")
	mustNil(t, Join(ctx, parts, joined))
	if g, w := blockSeq(trackBlocks(collectAllBlocks(t, joined))[matrix.VideoTrack]), blockSeq(video); g != w {
		t.Errorf("the join does not give the source back:\n got %s\nwant %s", g, w)
	}
}
