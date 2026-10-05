package mp4_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/ops"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mp4"
)

// subNames lists every subtitle resource of the plan, in the given order of
// segments.
func subNames(plan *mp4.HLSPlan, order func(n int) []int) []string {
	var names []string
	for k := 1; k <= 2; k++ {
		for _, n := range order(plan.NumSegments()) {
			names = append(names, fmt.Sprintf("sub%d_%05d.vtt", k, n))
		}
		names = append(names, fmt.Sprintf("sub%d.vtt", k))
	}
	return names
}

var subOrders = map[string]func(n int) []int{
	"playback order": func(n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = i + 1
		}
		return out
	},
	"backwards": func(n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = n - i
		}
		return out
	},
	"a seek to the middle, then around": func(n int) []int {
		out := []int{n / 2, n/2 + 1, n / 4, n, 1, n/2 + 2, 3 * n / 4, 2}
		for i := 1; i <= n; i++ {
			out = append(out, i)
		}
		return out
	},
}

// A plan given the source's subtitle index serves the very WebVTT the plan
// without it serves - every segment, every whole-track file, in whatever order
// they are asked for - and reaches the cues by their positions instead of
// walking the clusters. Both cue shapes are covered: cues with durations (the
// seek path) and cues that end on their successor (the prefix path).
func TestPlanHLSSubtitleIndexServesTheSameWebVTT(t *testing.T) {
	ctx := context.Background()
	for name, build := range map[string]func(testing.TB) string{
		"cues with durations":           mp4.BuildSeekFixtureForTest,
		"cues that end on the next cue": mp4.BuildSubCursorFixtureForTest,
	} {
		src := build(t)
		ix, err := ops.BuildSubtitleIndex(ctx, src, nil)
		if err != nil {
			t.Fatal(err)
		}
		for orderName, order := range subOrders {
			t.Run(name+"/"+orderName, func(t *testing.T) {
				walked, err := mp4.PlanHLS(ctx, src, mp4.Options{SegmentMs: 2000})
				if err != nil {
					t.Fatal(err)
				}
				indexed, err := mp4.PlanHLS(ctx, src, mp4.Options{SegmentMs: 2000, SubtitleIndex: ix})
				if err != nil {
					t.Fatal(err)
				}
				for _, res := range subNames(walked, order) {
					want, _, werr := walked.Resource(ctx, res)
					got, _, gerr := indexed.Resource(ctx, res)
					if werr != nil || gerr != nil {
						t.Fatalf("%s: walked %v, indexed %v", res, werr, gerr)
					}
					if !bytes.Equal(got, want) {
						t.Fatalf("%s differs with the index:\n%s\n--- without:\n%s", res, got, want)
					}
				}
				if st := indexed.Stats(); st.SubtitleWalks != 0 || st.SubtitleIndexedBlocks == 0 {
					t.Errorf("with the index: %d walks, %d blocks read by position, want no walk", st.SubtitleWalks, st.SubtitleIndexedBlocks)
				}
				if st := walked.Stats(); st.SubtitleWalks == 0 || st.SubtitleIndexedBlocks != 0 {
					t.Errorf("without the index: %d walks, %d blocks read by position", st.SubtitleWalks, st.SubtitleIndexedBlocks)
				}
			})
		}
	}
}

type countingFS struct{ bytes int64 }

type countingFile struct {
	*os.File
	n *int64
}

func (f countingFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	*f.n += int64(n)
	return n, err
}

func (c *countingFS) fs() *mkv.FS {
	return &mkv.FS{Open: func(path string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return countingFile{File: f, n: &c.bytes}, nil
	}}
}

// The point of the index: a subtitle segment asked for after a seek reads the
// few blocks it needs, not minutes of the file around them.
func TestPlanHLSSubtitleIndexReadsFarLessAfterASeek(t *testing.T) {
	ctx := context.Background()
	src := mp4.BuildSeekFixtureForTest(t)
	ix, err := ops.BuildSubtitleIndex(ctx, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	read := func(index mp4.SubtitleBlockIndex) int64 {
		var c countingFS
		plan, err := mp4.PlanHLS(ctx, src, mp4.Options{SegmentMs: 2000, SubtitleIndex: index, FS: c.fs()})
		if err != nil {
			t.Fatal(err)
		}
		c.bytes = 0
		if _, _, err := plan.Resource(ctx, fmt.Sprintf("sub1_%05d.vtt", plan.NumSegments()/2)); err != nil {
			t.Fatal(err)
		}
		return c.bytes
	}
	walked, indexed := read(nil), read(ix)
	if walked < 256<<10 {
		t.Fatalf("the walk read only %d bytes: the fixture no longer exercises a long scan", walked)
	}
	if indexed*5 > walked {
		t.Errorf("%d bytes read with the index, %d without: want less than a fifth", indexed, walked)
	}
}

// shiftedIndex is an index whose recorded timecodes are wrong: it passes the
// fingerprint and fails on the first block read.
type shiftedIndex struct{ *ops.SubtitleIndex }

func (s shiftedIndex) TrackBlocks(id uint64) []reader.IndexedBlock {
	blocks := s.SubtitleIndex.TrackBlocks(id)
	for i := range blocks {
		blocks[i].TimeMs += 7
	}
	return blocks
}

// An index that is not this file's costs its saving, never a wrong cue: one
// built from another file is set aside when the plan is made, one whose
// positions do not hold what it recorded is set aside on the first block that
// says so - and the subtitles are served by the walk, identical.
func TestPlanHLSSubtitleIndexStaleFallsBackToTheWalk(t *testing.T) {
	ctx := context.Background()
	src := mp4.BuildSeekFixtureForTest(t)
	other, err := ops.BuildSubtitleIndex(ctx, mp4.BuildSubCursorFixtureForTest(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	own, err := ops.BuildSubtitleIndex(ctx, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	walked, err := mp4.PlanHLS(ctx, src, mp4.Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	for name, index := range map[string]mp4.SubtitleBlockIndex{
		"an index built from another file":      other,
		"an index whose timecodes do not match": shiftedIndex{own},
	} {
		plan, err := mp4.PlanHLS(ctx, src, mp4.Options{SegmentMs: 2000, SubtitleIndex: index})
		if err != nil {
			t.Fatalf("%s: PlanHLS: %v", name, err)
		}
		for _, res := range subNames(walked, subOrders["a seek to the middle, then around"]) {
			want, _, werr := walked.Resource(ctx, res)
			got, _, gerr := plan.Resource(ctx, res)
			if werr != nil || gerr != nil || !bytes.Equal(got, want) {
				t.Fatalf("%s: %s differs from the walk (%v, %v)", name, res, werr, gerr)
			}
		}
		if st := plan.Stats(); st.SubtitleWalks == 0 || st.SubtitleIndexedBlocks != 0 {
			t.Errorf("%s: %d walks, %d blocks kept from the index, want the walk alone", name, st.SubtitleWalks, st.SubtitleIndexedBlocks)
		}
	}
}
