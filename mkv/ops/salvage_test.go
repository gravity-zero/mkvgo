package ops

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// salvageFixtureBlocks builds enough clusters (one per 1000ms bucket) with a
// video keyframe leading each, so Salvage's cue rebuild always has a target.
func salvageFixtureBlockSets(n int) [][]mkv.Block {
	sets := make([][]mkv.Block, 0, n)
	for i := 0; i < n; i++ {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0xAA, 0xBB, 0xCC, 0xDD}},
			{TrackNumber: 2, Timecode: ts, Keyframe: true, Data: []byte{0x01, 0x02}},
		})
	}
	return sets
}

func salvageFixture(t *testing.T, dir, name string, n int) string {
	t.Helper()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	return buildMultiClusterMKV(t, dir, name, tracks, salvageFixtureBlockSets(n), int64(n*1000))
}

// readAll returns the full contents of path.
func readAll(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeAll(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// blockReaderIteratesCleanly proves the output file's cluster stream decodes
// end to end through BlockReader without error.
func blockReaderIteratesCleanly(t *testing.T, path string) int {
	t.Helper()
	c, err := reader.OpenWithFS(context.Background(), path, nil)
	if err != nil {
		t.Fatalf("open result: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		t.Fatalf("new block reader: %v", err)
	}
	n := 0
	for {
		_, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("block reader iteration failed at block %d: %v", n, err)
		}
		n++
	}
	return n
}

func validateErrorFree(t *testing.T, path string) {
	t.Helper()
	issues, err := Validate(context.Background(), path)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, is := range issues {
		if is.Severity == mkv.SeverityError {
			t.Errorf("validate reported an error: %s", is.Message)
		}
	}
}

func TestSalvage_CleanFile(t *testing.T) {
	dir := t.TempDir()
	src := salvageFixture(t, dir, "clean.mkv", 6)
	dst := filepath.Join(dir, "clean.out.mkv")

	report, err := Salvage(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("Salvage: %v", err)
	}
	if len(report.DamagedRanges) != 0 {
		t.Errorf("clean source should have zero damaged ranges, got %d", len(report.DamagedRanges))
	}
	if report.BytesSkipped != 0 {
		t.Errorf("clean source should skip 0 bytes, got %d", report.BytesSkipped)
	}
	if report.ClustersCopied != 6 {
		t.Errorf("ClustersCopied = %d, want 6", report.ClustersCopied)
	}
	if report.BytesCopied <= 0 {
		t.Errorf("BytesCopied should be positive, got %d", report.BytesCopied)
	}

	blockReaderIteratesCleanly(t, dst)
	validateErrorFree(t, dst)

	diffs, err := CompareBlocks(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("CompareBlocks: %v", err)
	}
	if len(diffs) != 0 {
		t.Errorf("clean salvage should be byte-identical media content, got diffs: %v", diffs)
	}
}

// TestSalvage_ZeroedRegion zeros a byte range spanning a cluster boundary
// (destroying the element header there) and checks Salvage resyncs past it.
func TestSalvage_ZeroedRegion(t *testing.T) {
	dir := t.TempDir()
	src := salvageFixture(t, dir, "zeroed.mkv", 10)
	data := readAll(t, src)

	// Find the byte offset of the 4th cluster's ID (0x1F43B675) by scanning,
	// then zero its ID and declared size (plus a few bytes of its opening
	// child) so the top-level walk cannot decode it as an element at all -
	// this is a corruption a resync must recover from, not a mere content
	// change. The preceding cluster is left untouched.
	magic := []byte{0x1F, 0x43, 0xB6, 0x75}
	offsets := findAll(data, magic)
	if len(offsets) < 5 {
		t.Fatalf("fixture has only %d clusters, need >=5", len(offsets))
	}
	zeroStart := offsets[3]
	zeroEnd := offsets[3] + 10
	for i := zeroStart; i < zeroEnd; i++ {
		data[i] = 0
	}
	corrupted := filepath.Join(dir, "zeroed.corrupt.mkv")
	writeAll(t, corrupted, data)

	dst := filepath.Join(dir, "zeroed.out.mkv")
	report, err := Salvage(context.Background(), corrupted, dst)
	if err != nil {
		t.Fatalf("Salvage: %v", err)
	}
	if len(report.DamagedRanges) == 0 {
		t.Fatal("expected at least one damaged range")
	}
	first, last := report.DamagedRanges[0], report.DamagedRanges[len(report.DamagedRanges)-1]
	if first.StartOffset > zeroStart || last.EndOffset < zeroEnd {
		t.Errorf("reported damage [%d,%d) does not cover the zeroed region [%d,%d)", first.StartOffset, last.EndOffset, zeroStart, zeroEnd)
	}

	n := blockReaderIteratesCleanly(t, dst)
	if n == 0 {
		t.Error("expected some blocks to survive")
	}
	validateErrorFree(t, dst)
}

// findFirstSimpleBlockOffset parses the Cluster at clusterAt (an absolute
// offset into data) and returns the absolute offset of its first
// SimpleBlock's element ID byte.
func findFirstSimpleBlockOffset(t *testing.T, data []byte, clusterAt int64) int64 {
	t.Helper()
	total := int64(len(data)) - clusterAt
	br := bytes.NewReader(data[clusterAt:])
	pos := func() int64 { return clusterAt + total - int64(br.Len()) }

	h, _, err := ebml.ReadElementHeader(br)
	if err != nil || h.ID != mkv.IDCluster {
		t.Fatalf("expected Cluster at %d, got id=0x%X err=%v", clusterAt, h.ID, err)
	}
	bodyEnd := pos() + h.Size
	for pos() < bodyEnd {
		childStart := pos()
		ch, _, err := ebml.ReadElementHeader(br)
		if err != nil {
			t.Fatalf("child header decode failed: %v", err)
		}
		if ch.ID == mkv.IDSimpleBlock {
			return childStart
		}
		if _, err := br.Seek(ch.Size, io.SeekCurrent); err != nil {
			t.Fatalf("seek child body: %v", err)
		}
	}
	t.Fatalf("no SimpleBlock found in cluster at %d", clusterAt)
	return -1
}

// TestSalvage_GarbageInsideCluster corrupts the element ID byte of a
// SimpleBlock strictly inside one cluster's body (the Cluster's own ID/size
// untouched) and checks the surgical recovery keeps everything except that
// one dead block: the cluster is split around it, its second block survives,
// and the damage reported is the block's bytes - not the whole cluster.
func TestSalvage_GarbageInsideCluster(t *testing.T) {
	dir := t.TempDir()
	src := salvageFixture(t, dir, "garbage.mkv", 8)
	data := readAll(t, src)

	magic := []byte{0x1F, 0x43, 0xB6, 0x75}
	offsets := findAll(data, magic)
	if len(offsets) < 4 {
		t.Fatalf("fixture has only %d clusters, need >=4", len(offsets))
	}
	blockAt := findFirstSimpleBlockOffset(t, data, offsets[2])
	data[blockAt] = 0x00 // invalid EBML ID leading byte: guaranteed header decode failure

	corrupted := filepath.Join(dir, "garbage.corrupt.mkv")
	writeAll(t, corrupted, data)

	dst := filepath.Join(dir, "garbage.out.mkv")
	report, err := Salvage(context.Background(), corrupted, dst)
	if err != nil {
		t.Fatalf("Salvage: %v", err)
	}
	if report.ClustersCopied != 8 {
		t.Errorf("ClustersCopied = %d, want 8 (the damaged cluster is recovered, minus one block)", report.ClustersCopied)
	}
	if len(report.DamagedRanges) != 1 {
		t.Fatalf("expected exactly 1 damaged range, got %d", len(report.DamagedRanges))
	}
	dr := report.DamagedRanges[0]
	if dr.StartOffset != blockAt || dr.EndOffset >= offsets[3] {
		t.Errorf("damaged range = [%d,%d), want just the dead block starting at %d (well before the next cluster at %d)",
			dr.StartOffset, dr.EndOffset, blockAt, offsets[3])
	}
	if len(report.RepairedRanges) != 1 {
		t.Fatalf("expected exactly 1 repaired range, got %d", len(report.RepairedRanges))
	}
	if rr := report.RepairedRanges[0]; rr.StartOffset != offsets[2] || rr.EndOffset != offsets[3] {
		t.Errorf("repaired range = [%d,%d), want the whole cluster region [%d,%d)", rr.StartOffset, rr.EndOffset, offsets[2], offsets[3])
	}
	// 8 clusters x 2 blocks, minus the one dead block.
	if n := blockReaderIteratesCleanly(t, dst); n != 8*2-1 {
		t.Errorf("surviving blocks = %d, want 15", n)
	}
	validateErrorFree(t, dst)
}

// TestSalvage_TruncatedTail truncates the file mid-cluster and checks Salvage
// reports one damaged range to EOF and still writes a playable prefix.
func TestSalvage_TruncatedTail(t *testing.T) {
	dir := t.TempDir()
	src := salvageFixture(t, dir, "trunc.mkv", 8)
	data := readAll(t, src)

	magic := []byte{0x1F, 0x43, 0xB6, 0x75}
	offsets := findAll(data, magic)
	if len(offsets) < 6 {
		t.Fatalf("fixture has only %d clusters, need >=6", len(offsets))
	}
	// Cut off partway through the 6th cluster's body.
	cutAt := offsets[5] + 15
	truncated := filepath.Join(dir, "trunc.corrupt.mkv")
	writeAll(t, truncated, data[:cutAt])

	dst := filepath.Join(dir, "trunc.out.mkv")
	report, err := Salvage(context.Background(), truncated, dst)
	if err != nil {
		t.Fatalf("Salvage: %v", err)
	}
	if len(report.DamagedRanges) != 1 {
		t.Fatalf("expected exactly 1 damaged range (truncated tail), got %d", len(report.DamagedRanges))
	}
	dr := report.DamagedRanges[0]
	if dr.EndOffset != int64(cutAt) {
		t.Errorf("damaged range should end at EOF (%d), got %d", cutAt, dr.EndOffset)
	}
	if report.ClustersCopied != 5 {
		t.Errorf("ClustersCopied = %d, want 5 (clusters before the cut)", report.ClustersCopied)
	}
	blockReaderIteratesCleanly(t, dst)
	validateErrorFree(t, dst)
}

// TestSalvage_ResyncCapExceeded shrinks the resync cap for the duration of
// the test and wipes a run of clusters wider than it, followed by more real
// data - Salvage must fail cleanly, not hang or silently truncate.
func TestSalvage_ResyncCapExceeded(t *testing.T) {
	orig := salvageResyncCap
	salvageResyncCap = 128
	defer func() { salvageResyncCap = orig }()

	dir := t.TempDir()
	// Big per-block payloads so each cluster is comfortably wider than the
	// shrunk cap; zeroing three of them in a row guarantees no clusterMagic
	// survives within any 128-byte scan window.
	tracks := []mkv.Track{videoTrack(1)}
	blockSets := make([][]mkv.Block, 0, 6)
	for i := 0; i < 6; i++ {
		blockSets = append(blockSets, []mkv.Block{
			{TrackNumber: 1, Timecode: int64(i * 1000), Keyframe: true, Data: bytes.Repeat([]byte{0xAB}, 300)},
		})
	}
	src := buildMultiClusterMKV(t, dir, "cap.mkv", tracks, blockSets, 6000)
	data := readAll(t, src)

	magic := []byte{0x1F, 0x43, 0xB6, 0x75}
	offsets := findAll(data, magic)
	if len(offsets) < 5 {
		t.Fatalf("fixture has only %d clusters, need >=5", len(offsets))
	}
	// Wipe clusters 2, 3 and 4 in place (same length, no insert), leaving
	// cluster 5 and the trailing Cues intact well beyond the shrunk cap.
	for i := offsets[1]; i < offsets[4]; i++ {
		data[i] = 0
	}
	if offsets[4]-offsets[1] <= salvageResyncCap {
		t.Fatalf("wiped span %d must exceed the shrunk cap %d", offsets[4]-offsets[1], salvageResyncCap)
	}
	corrupted := filepath.Join(dir, "cap.corrupt.mkv")
	writeAll(t, corrupted, data)

	dst := filepath.Join(dir, "cap.out.mkv")
	done := make(chan struct{})
	var report *SalvageReport
	var err error
	go func() {
		report, err = Salvage(context.Background(), corrupted, dst)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Salvage hung past the resync cap")
	}
	if err == nil {
		t.Fatalf("expected an error when the resync scan cap is exceeded, got report %+v", report)
	}
}

// findAll returns every offset in data where pattern occurs.
func findAll(data, pattern []byte) []int64 {
	var out []int64
	from := 0
	for from < len(data) {
		rel := bytes.Index(data[from:], pattern)
		if rel < 0 {
			return out
		}
		out = append(out, int64(from+rel))
		from = from + rel + 1
	}
	return out
}
