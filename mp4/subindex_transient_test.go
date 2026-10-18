package mp4_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/ops"
	"github.com/gravity-zero/mkvgo/mp4"
)

type failArmedFile struct {
	*os.File
	armed *atomic.Bool
}

func (f failArmedFile) Read(p []byte) (int, error) {
	if f.armed.CompareAndSwap(true, false) {
		return 0, errors.New("connection reset")
	}
	return f.File.Read(p)
}

// TestSubtitleIndexSurvivesAFailedRead pins that a read failing once while the index serves a subtitle segment
// sends that request to the walk, and the next one back to the index, instead of dropping the index for good.
func TestSubtitleIndexSurvivesAFailedRead(t *testing.T) {
	ctx := context.Background()
	src := mp4.BuildSeekFixtureForTest(t)
	ix, err := ops.BuildSubtitleIndex(ctx, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	var armed atomic.Bool
	fs := &mkv.FS{Open: func(path string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return failArmedFile{f, &armed}, nil
	}, Stat: os.Stat}
	plan, err := mp4.PlanHLS(ctx, src, mp4.Options{SegmentMs: 2000, SubtitleIndex: ix, FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	_, _, _ = plan.Resource(ctx, fmt.Sprintf("sub1_%05d.vtt", plan.NumSegments()/3))
	before := plan.Stats()
	if _, _, err := plan.Resource(ctx, fmt.Sprintf("sub1_%05d.vtt", plan.NumSegments()-1)); err != nil {
		t.Fatal(err)
	}
	after := plan.Stats()
	if after.SubtitleIndexedBlocks == before.SubtitleIndexedBlocks || after.SubtitleWalks != before.SubtitleWalks {
		t.Errorf("after one failed read the index served %d blocks and the walk ran %d times: the index was dropped",
			after.SubtitleIndexedBlocks-before.SubtitleIndexedBlocks, after.SubtitleWalks-before.SubtitleWalks)
	}
}
