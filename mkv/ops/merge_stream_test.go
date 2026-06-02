package ops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// writeTrackMKV writes a seekable single-track MKV with one block per timecode,
// grouped into <=1s clusters so relative timecodes stay in int16 range.
func writeTrackMKV(t *testing.T, path, codec string, timecodes []int64) {
	t.Helper()
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: codec}}

	var seg bytes.Buffer
	mustNil(t, writer.WriteSegmentInfo(&seg, &info, 0))
	mustNil(t, writer.WriteTracks(&seg, tracks))
	for i := 0; i < len(timecodes); {
		base := timecodes[i]
		var blocks []mkv.Block
		for i < len(timecodes) && timecodes[i]-base < 1000 {
			blocks = append(blocks, mkv.Block{
				TrackNumber: 1, Timecode: timecodes[i], Keyframe: true,
				Data: []byte{byte(timecodes[i])},
			})
			i++
		}
		mustNil(t, writer.WriteCluster(&seg, base, info.TimecodeScale, blocks))
	}

	var buf bytes.Buffer
	mustNil(t, writer.WriteEBMLHeader(&buf))
	mustNil(t, writer.WriteMasterElement(&buf, mkv.IDSegment, seg.Bytes()))
	mustNil(t, os.WriteFile(path, buf.Bytes(), 0o644))
}

func collectAllBlocks(t *testing.T, path string) []mkv.Block {
	t.Helper()
	c, err := reader.Open(context.Background(), path)
	mustNil(t, err)
	f, err := os.Open(path)
	mustNil(t, err)
	defer f.Close()
	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	mustNil(t, err)
	var out []mkv.Block
	for {
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		mustNil(t, err)
		out = append(out, b)
	}
	return out
}

func assertMonotonic(t *testing.T, blocks []mkv.Block) {
	t.Helper()
	for i := 1; i < len(blocks); i++ {
		if blocks[i].Timecode < blocks[i-1].Timecode {
			t.Errorf("blocks not timecode-ordered at %d: %d after %d", i, blocks[i].Timecode, blocks[i-1].Timecode)
		}
	}
}

// TestMuxStreamMergePreservesAndInterleaves checks the streaming k-way merge:
// every block from every source survives, tracks are remapped, and the output is
// ordered by timecode across sources -- including across cluster boundaries.
func TestMuxStreamMergePreservesAndInterleaves(t *testing.T) {
	dir := t.TempDir()
	srcA := filepath.Join(dir, "a.mkv")
	srcB := filepath.Join(dir, "b.mkv")
	// interleaved across sources and spanning multiple 1s clusters.
	writeTrackMKV(t, srcA, "vp9", []int64{0, 100, 1200, 2400})
	writeTrackMKV(t, srcB, "opus", []int64{50, 1100, 1300, 2000})
	dst := filepath.Join(dir, "muxed.mkv")

	mustNil(t, Mux(context.Background(), mkv.MuxOptions{
		OutputPath: dst,
		Tracks: []mkv.TrackInput{
			{SourcePath: srcA, TrackID: 1},
			{SourcePath: srcB, TrackID: 1},
		},
	}))

	blocks := collectAllBlocks(t, dst)

	if len(blocks) != 8 {
		t.Fatalf("got %d blocks, want 8 (all preserved across both sources)", len(blocks))
	}
	counts := map[uint64]int{}
	for _, b := range blocks {
		counts[b.TrackNumber]++
	}
	if counts[1] != 4 || counts[2] != 4 {
		t.Errorf("per-track counts = %v, want map[1:4 2:4]", counts)
	}
	assertMonotonic(t, blocks)

	want := []int64{0, 50, 100, 1100, 1200, 1300, 2000, 2400}
	for i, b := range blocks {
		if b.Timecode != want[i] {
			t.Errorf("block %d timecode = %d, want %d (merge order)", i, b.Timecode, want[i])
		}
	}
}

// TestAddTrackStreamMergeInterleaves checks that AddTrack (the 2-source variant)
// interleaves the added track's blocks with the base file's by timecode and
// keeps every block.
func TestAddTrackStreamMergeInterleaves(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.mkv")
	add := filepath.Join(dir, "add.mkv")
	writeTrackMKV(t, base, "vp9", []int64{0, 100, 200})
	writeTrackMKV(t, add, "opus", []int64{50, 150, 250})
	dst := filepath.Join(dir, "out.mkv")

	mustNil(t, AddTrack(context.Background(), base, dst, mkv.TrackInput{
		SourcePath: add, TrackID: 1, Language: "eng",
	}))

	blocks := collectAllBlocks(t, dst)
	if len(blocks) != 6 {
		t.Fatalf("got %d blocks, want 6 (base + added)", len(blocks))
	}
	counts := map[uint64]int{}
	for _, b := range blocks {
		counts[b.TrackNumber]++
	}
	if counts[1] != 3 || counts[2] != 3 {
		t.Errorf("per-track counts = %v, want map[1:3 2:3]", counts)
	}
	assertMonotonic(t, blocks)
}
