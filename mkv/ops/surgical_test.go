package ops

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestMapDamage_DryRun checks the dry-run walk reports exactly what the real
// salvage would, without writing anything.
func TestMapDamage_DryRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := salvageFixture(t, dir, "map.mkv", 8)
	data := readAll(t, src)

	magic := []byte{0x1F, 0x43, 0xB6, 0x75}
	offsets := findAll(data, magic)
	if len(offsets) < 5 {
		t.Fatalf("fixture has only %d clusters, need >=5", len(offsets))
	}
	junk := bytes.Repeat([]byte{0x00, 0xFF, 0x51}, 33)
	corrupted := filepath.Join(dir, "map.corrupt.mkv")
	writeAll(t, corrupted, spliceAt(data, offsets[3], junk))

	mapped, err := MapDamage(ctx, corrupted)
	if err != nil {
		t.Fatalf("MapDamage: %v", err)
	}

	dst := filepath.Join(dir, "map.out.mkv")
	real, err := Salvage(ctx, corrupted, dst)
	if err != nil {
		t.Fatalf("Salvage: %v", err)
	}

	if len(mapped.DamagedRanges) != len(real.DamagedRanges) {
		t.Fatalf("dry-run found %d damaged ranges, real salvage %d", len(mapped.DamagedRanges), len(real.DamagedRanges))
	}
	for i := range mapped.DamagedRanges {
		if mapped.DamagedRanges[i] != real.DamagedRanges[i] {
			t.Errorf("damaged range %d differs: dry-run %+v, real %+v", i, mapped.DamagedRanges[i], real.DamagedRanges[i])
		}
	}
	if mapped.BytesSkipped != real.BytesSkipped || mapped.ClustersCopied != real.ClustersCopied {
		t.Errorf("dry-run report (skipped=%d clusters=%d) differs from real (skipped=%d clusters=%d)",
			mapped.BytesSkipped, mapped.ClustersCopied, real.BytesSkipped, real.ClustersCopied)
	}
}

// cleanCutFixture builds clusters whose video has a keyframe followed by
// dependent frames, so a mid-cluster gap leaves reference-less video behind.
func cleanCutFixture(t *testing.T, dir, name string, n int) string {
	t.Helper()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	sets := make([][]mkv.Block, 0, n)
	for i := 0; i < n; i++ {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0xAA, 0xBB, 0xCC, 0xDD}},
			{TrackNumber: 1, Timecode: ts + 100, Keyframe: false, Data: []byte{0x11, 0x12}},
			{TrackNumber: 1, Timecode: ts + 200, Keyframe: false, Data: []byte{0x13, 0x14}},
			{TrackNumber: 2, Timecode: ts + 300, Keyframe: true, Data: []byte{0x01, 0x02}},
		})
	}
	return buildMultiClusterMKV(t, dir, name, tracks, sets, int64(n*1000))
}

// TestSalvage_CleanCut kills a cluster's video keyframe and checks that with
// Options.CleanCut the recovered dependent video frames before the next
// keyframe are dropped (audio kept), while without the option they survive.
func TestSalvage_CleanCut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := cleanCutFixture(t, dir, "cut.mkv", 6)
	data := readAll(t, src)

	magic := []byte{0x1F, 0x43, 0xB6, 0x75}
	offsets := findAll(data, magic)
	if len(offsets) < 4 {
		t.Fatalf("fixture has only %d clusters, need >=4", len(offsets))
	}
	blockAt := findFirstSimpleBlockOffset(t, data, offsets[2])
	data[blockAt] = 0x00 // kill cluster 3's video keyframe
	corrupted := filepath.Join(dir, "cut.corrupt.mkv")
	writeAll(t, corrupted, data)

	// Without clean cut: the two dependent video frames are recovered.
	plain := filepath.Join(dir, "cut.plain.mkv")
	plainReport, err := Salvage(ctx, corrupted, plain)
	if err != nil {
		t.Fatalf("Salvage: %v", err)
	}
	if plainReport.CleanCutBytes != 0 {
		t.Errorf("CleanCutBytes = %d without the option, want 0", plainReport.CleanCutBytes)
	}
	plainBlocks := blockReaderIteratesCleanly(t, plain)

	// With clean cut: they are dropped up to the next video keyframe.
	cut := filepath.Join(dir, "cut.clean.mkv")
	cutReport, err := Salvage(ctx, corrupted, cut, mkv.Options{CleanCut: true})
	if err != nil {
		t.Fatalf("Salvage with CleanCut: %v", err)
	}
	if cutReport.CleanCutBytes == 0 {
		t.Error("expected CleanCutBytes > 0: dependent video frames follow the gap")
	}
	cutBlocks := blockReaderIteratesCleanly(t, cut)
	if want := plainBlocks - 2; cutBlocks != want {
		t.Errorf("clean-cut output has %d blocks, want %d (the 2 dependent video frames dropped)", cutBlocks, want)
	}
	validateErrorFree(t, cut)

	// The damaged range's end time must extend to the resume keyframe (the
	// next cluster's keyframe at 3000ms).
	if n := len(cutReport.DamagedRanges); n != 1 {
		t.Fatalf("expected 1 damaged range, got %d", n)
	}
	if end := cutReport.DamagedRanges[0].ApproxEndMs; end < 3000 {
		t.Errorf("clean-cut damaged range ends at %dms, want >= 3000 (the resume keyframe)", end)
	}
}

// FuzzSurgicalScanCluster hammers the surgical scanner with arbitrary bytes:
// it must never panic, and any outcome it returns must be internally
// consistent (ordered runs and gaps within bounds).
func FuzzSurgicalScanCluster(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0xE7, 0x81, 0x00})                                                  // bare timestamp
	f.Add([]byte{0xE7, 0x81, 0x00, 0xA3, 0x85, 0x81, 0x00, 0x00, 0x80, 0xFF})        // ts + one block
	f.Add(append([]byte{0xE7, 0x81, 0x00}, bytes.Repeat([]byte{0xA3, 0x00}, 32)...)) // ts + broken blocks
	f.Add([]byte{0xE7, 0x81, 0x00, 0x1F, 0x43, 0xB6, 0x75, 0x83, 0xE7, 0x81, 0x01})  // ts + next cluster

	f.Fuzz(func(t *testing.T, data []byte) {
		raw := bytes.NewReader(data)
		out, err := surgicalScanCluster(raw, 0, int64(len(data)), nil, 1000)
		if err != nil || out == nil {
			return
		}
		if out.end < 0 || out.end > int64(len(data)) {
			t.Fatalf("end %d out of bounds (len %d)", out.end, len(data))
		}
		prev := int64(0)
		for _, r := range out.runs {
			if r.start < prev || r.end < r.start || r.end > int64(len(data)) {
				t.Fatalf("run [%d,%d) out of order or bounds (prev end %d)", r.start, r.end, prev)
			}
			prev = r.end
		}
		for _, g := range out.gaps {
			if g.StartOffset < 0 || g.EndOffset < g.StartOffset || g.EndOffset > int64(len(data)) {
				t.Fatalf("gap [%d,%d) out of bounds", g.StartOffset, g.EndOffset)
			}
		}
	})
}
