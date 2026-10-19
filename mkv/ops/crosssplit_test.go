package ops

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/matrix"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestJoinOfPartsFromTwoSplits pins that parts of two splits of one source cut at other points are no continuation
// of each other: joined, they play end to end, every block and cue kept, no chapter merged across their seam, while
// the parts of one split still give the source back.
func TestJoinOfPartsFromTwoSplits(t *testing.T) {
	ctx := context.Background()
	c := matrix.Case{Audio: true, Subtitle: matrix.CueOverCut, Chapters: matrix.EndedChapters, Cuts: 1}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	mustNil(t, c.BuildMKV(src))
	splitAt := func(name string, ms int64) []string {
		parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, name), Ranges: []mkv.TimeRange{{EndMs: ms}, {StartMs: ms}}})
		mustNil(t, err)
		return parts
	}
	a, b := splitAt("a", 3000), splitAt("b", 2000)
	for name, parts := range map[string][]string{"a0+b1": {a[0], b[1]}, "b1+a0": {b[1], a[0]}, "a1+a1": {a[1], a[1]}} {
		joined := filepath.Join(dir, name+".mkv")
		mustNil(t, Join(ctx, parts, joined))
		want := map[uint64]int{}
		var chapters int
		for _, p := range parts {
			for id, bs := range trackBlocks(collectAllBlocks(t, p)) {
				want[id] += len(bs)
			}
			pc, err := reader.Open(ctx, p)
			mustNil(t, err)
			chapters += len(pc.Chapters)
		}
		got := trackBlocks(collectAllBlocks(t, joined))
		for id, n := range want {
			if len(got[id]) != n {
				t.Errorf("%s: track %d holds %d blocks, its parts %d end to end", name, id, len(got[id]), n)
			}
		}
		jc, err := reader.Open(ctx, joined)
		mustNil(t, err)
		if len(jc.Chapters) != chapters {
			t.Errorf("%s: %d chapters, its parts %d", name, len(jc.Chapters), chapters)
		}
	}
	joined := filepath.Join(dir, "a0+a1.mkv")
	mustNil(t, Join(ctx, a, joined))
	got := trackBlocks(collectAllBlocks(t, joined))
	for id, want := range joinedBlocks(c) {
		if blockSeq(got[id]) != blockSeq(want) {
			t.Errorf("a0+a1: track %d does not give the source back", id)
		}
	}
}
