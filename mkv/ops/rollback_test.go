package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// rollbackRoundTrip runs ApplyRollback on the given delta and asserts the
// reconstruction is byte-identical to original.
func rollbackRoundTrip(t *testing.T, repaired string, delta []byte, original []byte) {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "restored.mkv")
	if err := ApplyRollback(context.Background(), repaired, bytes.NewReader(delta), dst); err != nil {
		t.Fatalf("ApplyRollback: %v", err)
	}
	got := readAll(t, dst)
	if !bytes.Equal(got, original) {
		t.Fatalf("reconstruction differs from the original (%d vs %d bytes)", len(got), len(original))
	}
}

// TestRollback_StrictReindexRoundTrip: clean file, strict reindex, delta must
// reconstruct the original byte for byte and stay tiny.
func TestRollback_StrictReindexRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := salvageFixture(t, dir, "clean.mkv", 10)
	original := readAll(t, src)

	var delta bytes.Buffer
	var infos []mkv.RollbackInfo
	dst := filepath.Join(dir, "reindexed.mkv")
	err := Reindex(ctx, src, dst, mkv.Options{
		RollbackSink: &delta,
		OnRollback:   func(i mkv.RollbackInfo) { infos = append(infos, i) },
	})
	if err != nil {
		t.Fatalf("Reindex with RollbackSink: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("OnRollback called %d times, want 1", len(infos))
	}
	if infos[0].Bytes != int64(delta.Len()) {
		t.Errorf("RollbackInfo.Bytes = %d, sink got %d", infos[0].Bytes, delta.Len())
	}
	if want := sha256.Sum256(original); infos[0].SrcSHA256 != want {
		t.Error("RollbackInfo.SrcSHA256 does not match the original file")
	}
	if want := sha256.Sum256(readAll(t, dst)); infos[0].DstSHA256 != want {
		t.Error("RollbackInfo.DstSHA256 does not match the repaired file")
	}

	rollbackRoundTrip(t, dst, delta.Bytes(), original)
}

// TestRollback_ResyncRoundTrip: damaged file (spliced junk + a surgical
// repair), the delta must reconstruct the DAMAGED original exactly -
// including the junk and the lying bytes the repair dropped or fixed.
func TestRollback_ResyncRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := salvageFixture(t, dir, "base.mkv", 8)
	data := readAll(t, base)

	magic := []byte{0x1F, 0x43, 0xB6, 0x75}
	offsets := findAll(data, magic)
	if len(offsets) < 5 {
		t.Fatalf("fixture has only %d clusters, need >=5", len(offsets))
	}
	// Splice junk between clusters AND kill a block inside another cluster,
	// so the delta covers both a resync gap and a surgical split.
	junk := bytes.Repeat([]byte{0x00, 0xFF, 0x51}, 33)
	blockAt := findFirstSimpleBlockOffset(t, data, offsets[1])
	data[blockAt] = 0x00
	corrupted := spliceAt(data, offsets[3], junk)
	src := filepath.Join(dir, "damaged.mkv")
	writeAll(t, src, corrupted)

	var delta bytes.Buffer
	dst := filepath.Join(dir, "repaired.mkv")
	err := Reindex(ctx, src, dst, mkv.Options{
		Resync:       true,
		RollbackSink: &delta,
	})
	if err != nil {
		t.Fatalf("Reindex Resync with RollbackSink: %v", err)
	}

	rollbackRoundTrip(t, dst, delta.Bytes(), corrupted)
}

// TestRollback_SalvageCleanCutRoundTrip: the clean-cut filter rewrites
// cluster bodies; the delta must still reconstruct the original.
func TestRollback_SalvageCleanCutRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := cleanCutFixture(t, dir, "cut.mkv", 6)
	data := readAll(t, src)

	magic := []byte{0x1F, 0x43, 0xB6, 0x75}
	offsets := findAll(data, magic)
	blockAt := findFirstSimpleBlockOffset(t, data, offsets[2])
	data[blockAt] = 0x00
	corrupted := filepath.Join(dir, "cut.corrupt.mkv")
	writeAll(t, corrupted, data)

	var delta bytes.Buffer
	dst := filepath.Join(dir, "cut.out.mkv")
	if _, err := Salvage(ctx, corrupted, dst, mkv.Options{CleanCut: true, RollbackSink: &delta}); err != nil {
		t.Fatalf("Salvage with CleanCut+RollbackSink: %v", err)
	}

	rollbackRoundTrip(t, dst, delta.Bytes(), data)
}

// TestRollback_DeltaIsTiny asserts the whole point: the delta stays well
// under 0.1% of the source on a realistic clean fixture.
func TestRollback_DeltaIsTiny(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Clusters with realistically meaty payloads (a real file's clusters are
	// MBs; the delta's per-cluster cost is a few dozen fixed bytes).
	tracks := []mkv.Track{videoTrack(1)}
	sets := make([][]mkv.Block, 0, 40)
	for i := 0; i < 40; i++ {
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: int64(i * 1000), Keyframe: true, Data: bytes.Repeat([]byte{0xAB}, 256<<10)},
		})
	}
	src := buildMultiClusterMKV(t, dir, "big.mkv", tracks, sets, 40000)
	srcSize := int64(len(readAll(t, src)))

	var delta bytes.Buffer
	dst := filepath.Join(dir, "big.out.mkv")
	if err := Reindex(ctx, src, dst, mkv.Options{RollbackSink: &delta}); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if ratio := float64(delta.Len()) / float64(srcSize); ratio > 0.001 {
		t.Errorf("delta is %d bytes for a %d-byte source (%.3f%%), want < 0.1%%", delta.Len(), srcSize, ratio*100)
	}
}

// TestRollback_InPlaceRoundTrip: the surgical in-place path persists its
// crash journal as a delta entry (identity COPY spans, journaled literals,
// TRUNC for the appended tail); the reconstruction must be byte-identical.
func TestRollback_InPlaceRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := salvageFixture(t, dir, "inplace.mkv", 8)
	original := readAll(t, target)

	var delta bytes.Buffer
	var infos []mkv.RollbackInfo
	err := ReindexInPlace(ctx, target, mkv.Options{
		RollbackSink:     &delta,
		RollbackRequired: true,
		OnRollback:       func(i mkv.RollbackInfo) { infos = append(infos, i) },
	})
	if err != nil {
		t.Fatalf("ReindexInPlace with RollbackSink: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("OnRollback called %d times, want 1", len(infos))
	}
	if want := sha256.Sum256(original); infos[0].SrcSHA256 != want {
		t.Error("SrcSHA256 does not match the pre-repair file")
	}
	if want := sha256.Sum256(readAll(t, target)); infos[0].DstSHA256 != want {
		t.Error("DstSHA256 does not match the repaired file")
	}

	rollbackRoundTrip(t, target, delta.Bytes(), original)
}

// TestRollback_Refusals: a modified repaired file, a corrupted crc and a
// truncated delta must all be refused, and no output may survive.
func TestRollback_Refusals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := salvageFixture(t, dir, "ref.mkv", 6)
	original := readAll(t, src)

	var delta bytes.Buffer
	dst := filepath.Join(dir, "ref.out.mkv")
	if err := Reindex(ctx, src, dst, mkv.Options{RollbackSink: &delta}); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	restored := filepath.Join(dir, "restored.mkv")

	// Repaired file modified by one byte.
	tampered := filepath.Join(dir, "tampered.mkv")
	tb := readAll(t, dst)
	tb[len(tb)/2] ^= 0x01
	writeAll(t, tampered, tb)
	if err := ApplyRollback(ctx, tampered, bytes.NewReader(delta.Bytes()), restored); err == nil {
		t.Error("ApplyRollback must refuse a modified repaired file")
	}

	// Entry crc corrupted (last byte of the delta).
	bad := append([]byte(nil), delta.Bytes()...)
	bad[len(bad)-1] ^= 0x01
	if err := ApplyRollback(ctx, dst, bytes.NewReader(bad), restored); err == nil {
		t.Error("ApplyRollback must refuse a corrupted entry crc")
	}

	// Truncated delta.
	if err := ApplyRollback(ctx, dst, bytes.NewReader(delta.Bytes()[:delta.Len()/2]), restored); err == nil {
		t.Error("ApplyRollback must refuse a truncated delta")
	}

	// No half-reconstructed file may survive a refusal.
	if _, err := os.Stat(restored); !os.IsNotExist(err) {
		t.Errorf("a refused rollback left an output file behind (stat err=%v)", err)
	}

	// And the intact delta still works.
	rollbackRoundTrip(t, dst, delta.Bytes(), original)
}

// FuzzApplyRollback hammers the entry parser with arbitrary bytes against a
// small valid repaired file: it must never panic and never claim success on
// garbage (a success requires the sha256 gates to pass, which fuzzed inputs
// cannot forge).
func FuzzApplyRollback(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("MKVGRB1\x00"))
	seed := make([]byte, 92)
	copy(seed, "MKVGRB1\x00")
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		repaired := filepath.Join(dir, "r.bin")
		if err := os.WriteFile(repaired, []byte("not much of a file"), 0o644); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, "out.bin")
		if err := ApplyRollback(context.Background(), repaired, bytes.NewReader(data), dst); err == nil {
			t.Fatal("ApplyRollback accepted fuzzed garbage")
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Fatalf("refused rollback left an output behind (stat err=%v)", err)
		}
	})
}
