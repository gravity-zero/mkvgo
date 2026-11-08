package mp4

import (
	"context"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// A subtitle segment's scan of the clusters ahead of its window asks the disk for its whole range first, as the
// media walk does for its window: scanning a minute of clusters header by header with no read-ahead, it cost the
// disk thousands of small reads and left cached pages that split the next windows' read-ahead (a 2 GB MKV served
// in full with its subtitle renditions: 10,966 disk reads of 184 KiB on a fresh plan, 2,823 of 713 KiB once the
// cursors were done; 2,811 without the subtitle renditions).
func TestSubtitleScanReadsAheadOverItsRange(t *testing.T) {
	type rng struct{ off, n int64 }
	var will []rng
	defer func(w func(*os.File, int64, int64)) { adviseWillNeed = w }(adviseWillNeed)
	adviseWillNeed = func(_ *os.File, off, n int64) { will = append(will, rng{off, n}) }
	defer func(f func(*os.File) bool) { networkSource = f }(networkSource)
	networkSource = func(*os.File) bool { return false }
	ctx := context.Background()
	plan, err := PlanHLS(ctx, "../internal/testdata/sample_subs.mkv", Options{SegmentMs: 2000, StreamFromFS: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.subs) != 1 {
		t.Fatalf("%d subtitle renditions, want 1", len(plan.subs))
	}
	if _, err := plan.subtitleSegment(ctx, 0, 0); err != nil {
		t.Fatal(err)
	}
	if len(will) == 0 {
		t.Fatal("the subtitle scan read nothing ahead")
	}
	lo, hi := will[0].off, will[0].off+will[0].n
	for _, w := range will[1:] {
		lo, hi = min(lo, w.off), max(hi, w.off+w.n)
	}
	// The scan starts at the window's cluster and runs to the end of its first stride: a 15 s file, so to its end.
	if lo > plan.offsets[0] || hi < plan.segEnd {
		t.Errorf("read ahead [%d, %d), want it to cover the scan [%d, %d)", lo, hi, plan.offsets[0], plan.segEnd)
	}
}

// fakeSubIndex is a SubtitleBlockIndex over one track's blocks, as a walk of the file lists them.
type fakeSubIndex struct {
	track  uint64
	blocks []reader.IndexedBlock
}

func (ix fakeSubIndex) Matches(int64, []byte, int64) bool { return true }
func (ix fakeSubIndex) TrackBlocks(id uint64) []reader.IndexedBlock {
	if id != ix.track {
		return nil
	}
	return ix.blocks
}

// TestIndexedSubtitleScanReadsAheadOverItsRange: served from a subtitle index, the scan seeks to each block
// instead of walking, a few KiB at scattered positions ahead of the media; it asks for the clusters it reaches
// into first, as the walk does, and reads no further ahead than the request needs (the 2 GB MKV above served its
// subtitles from the Cues: 10,949 disk reads of 184 KiB for a full read, 2,921 of 689 KiB with the read-ahead).
func TestIndexedSubtitleScanReadsAheadOverItsRange(t *testing.T) {
	const src = "../internal/testdata/sample_subs.mkv"
	ctx := context.Background()
	c, err := reader.Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		t.Fatal(err)
	}
	ix := fakeSubIndex{track: 2}
	for {
		b, err := br.Next()
		if err != nil {
			break
		}
		if b.TrackNumber == 2 {
			ix.blocks = append(ix.blocks, reader.IndexedBlock{Pos: br.Pos(), TimeMs: b.Timecode, Frames: 1})
		}
	}
	if len(ix.blocks) == 0 {
		t.Fatal("no subtitle block in the fixture")
	}
	type rng struct{ off, n int64 }
	var will []rng
	defer func(w func(*os.File, int64, int64)) { adviseWillNeed = w }(adviseWillNeed)
	adviseWillNeed = func(_ *os.File, off, n int64) { will = append(will, rng{off, n}) }
	defer func(f func(*os.File) bool) { networkSource = f }(networkSource)
	networkSource = func(*os.File) bool { return false }
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000, StreamFromFS: true, SubtitleIndex: ix})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.subtitleSegment(ctx, 0, 0); err != nil {
		t.Fatal(err)
	}
	if plan.stats.SubtitleIndexedBlocks == 0 {
		t.Fatal("the index did not serve the scan")
	}
	if len(will) == 0 {
		t.Fatal("the indexed scan read nothing ahead")
	}
	lo, hi := will[0].off, will[0].off+will[0].n
	for _, w := range will[1:] {
		lo, hi = min(lo, w.off), max(hi, w.off+w.n)
	}
	// The request needs the cues up to its window's end plus the relative-timecode spread (32 s on a 15 s file:
	// to the end), from the window's cluster.
	if lo > plan.offsets[0] || hi < plan.segEnd {
		t.Errorf("read ahead [%d, %d), want it to cover the scan [%d, %d)", lo, hi, plan.offsets[0], plan.segEnd)
	}
}
