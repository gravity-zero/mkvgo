package ops

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/matrix"
	"github.com/gravity-zero/mkvgo/mkv"
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
}
