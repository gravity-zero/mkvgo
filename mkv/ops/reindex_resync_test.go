package ops

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

// spliceAt returns data with junk inserted at offset (junk bytes pushed in
// between two elements, the classic "raw garbage between clusters" damage).
func spliceAt(data []byte, offset int64, junk []byte) []byte {
	out := make([]byte, 0, len(data)+len(junk))
	out = append(out, data[:offset]...)
	out = append(out, junk...)
	out = append(out, data[offset:]...)
	return out
}

// TestReindexResync_SplicedGarbage injects raw junk between two clusters:
// strict Reindex must refuse (with a pointer at the Resync option), Reindex
// with Options.Resync must skip exactly the junk, keep every cluster, pass
// DeepVerify, and report the dropped span through Options.OnSkip.
func TestReindexResync_SplicedGarbage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := salvageFixture(t, dir, "spliced.mkv", 8)
	data := readAll(t, src)

	magic := []byte{0x1F, 0x43, 0xB6, 0x75}
	offsets := findAll(data, magic)
	if len(offsets) < 5 {
		t.Fatalf("fixture has only %d clusters, need >=5", len(offsets))
	}
	junk := bytes.Repeat([]byte{0x00, 0xFF, 0x51}, 33) // no accidental cluster magic
	at := offsets[3]
	corrupted := filepath.Join(dir, "spliced.corrupt.mkv")
	writeAll(t, corrupted, spliceAt(data, at, junk))

	// Strict contract unchanged: the file is refused, and the error points
	// the operator at the opt-in.
	if err := Reindex(ctx, corrupted, filepath.Join(dir, "strict.out.mkv")); err == nil {
		t.Fatal("strict Reindex should refuse spliced garbage")
	} else if !strings.Contains(err.Error(), "Options.Resync") {
		t.Errorf("strict refusal should mention the Resync opt-in, got: %v", err)
	}

	var skipped []mkv.DamagedRange
	dst := filepath.Join(dir, "resync.out.mkv")
	err := Reindex(ctx, corrupted, dst, mkv.Options{
		Resync:     true,
		DeepVerify: true,
		OnSkip:     func(r mkv.DamagedRange) { skipped = append(skipped, r) },
	})
	if err != nil {
		t.Fatalf("Reindex with Resync: %v", err)
	}
	if len(skipped) != 1 {
		t.Fatalf("expected exactly 1 skipped range, got %d: %+v", len(skipped), skipped)
	}
	if skipped[0].StartOffset != at || skipped[0].EndOffset != at+int64(len(junk)) {
		t.Errorf("skipped range = [%d,%d), want exactly the junk [%d,%d)",
			skipped[0].StartOffset, skipped[0].EndOffset, at, at+int64(len(junk)))
	}
	if n := blockReaderIteratesCleanly(t, dst); n != 8*2 {
		t.Errorf("expected all 16 blocks to survive (junk was between clusters), got %d", n)
	}
	validateErrorFree(t, dst)
}

// TestReindexResync_OverdeclaredClusterSize corrupts a cluster's declared
// size so it overshoots its real extent (the walker would land mid-payload):
// the resync walk must drop the damaged span and still produce a valid file.
func TestReindexResync_OverdeclaredClusterSize(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := salvageFixture(t, dir, "overdecl.mkv", 12)
	data := readAll(t, src)

	magic := []byte{0x1F, 0x43, 0xB6, 0x75}
	offsets := findAll(data, magic)
	if len(offsets) < 8 {
		t.Fatalf("fixture has only %d clusters, need >=8", len(offsets))
	}
	// The fixture's clusters are tiny (1-byte size VINT right after the 4-byte
	// ID). Redeclare cluster 2's size as 126 (the 1-byte VINT maximum), which
	// swallows several following clusters' bytes.
	sizeAt := offsets[1] + 4
	if data[sizeAt]&0x80 == 0 {
		t.Fatalf("expected a 1-byte size VINT at %d, got 0x%02X", sizeAt, data[sizeAt])
	}
	data[sizeAt] = 0xFE
	corrupted := filepath.Join(dir, "overdecl.corrupt.mkv")
	writeAll(t, corrupted, data)

	var skipped []mkv.DamagedRange
	dst := filepath.Join(dir, "overdecl.out.mkv")
	err := Reindex(ctx, corrupted, dst, mkv.Options{
		Resync: true,
		OnSkip: func(r mkv.DamagedRange) { skipped = append(skipped, r) },
	})
	if err != nil {
		t.Fatalf("Reindex with Resync: %v", err)
	}
	if len(skipped) == 0 {
		t.Fatal("expected at least one skipped range")
	}
	if skipped[0].StartOffset != offsets[1] {
		t.Errorf("first skipped range starts at %d, want the corrupted cluster at %d", skipped[0].StartOffset, offsets[1])
	}
	if n := blockReaderIteratesCleanly(t, dst); n == 0 {
		t.Error("expected surviving blocks")
	}
	validateErrorFree(t, dst)
}

// TestReindexResync_CleanSourceMatchesStrict proves the opt-in is free on an
// intact file: byte-identical output to the strict path and zero OnSkip calls.
func TestReindexResync_CleanSourceMatchesStrict(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := salvageFixture(t, dir, "clean.mkv", 6)

	dstStrict := filepath.Join(dir, "strict.out.mkv")
	if err := Reindex(ctx, src, dstStrict); err != nil {
		t.Fatalf("strict Reindex: %v", err)
	}

	calls := 0
	dstResync := filepath.Join(dir, "resync.out.mkv")
	err := Reindex(ctx, src, dstResync, mkv.Options{
		Resync:     true,
		DeepVerify: true,
		OnSkip:     func(mkv.DamagedRange) { calls++ },
	})
	if err != nil {
		t.Fatalf("Reindex with Resync on a clean source: %v", err)
	}
	if calls != 0 {
		t.Errorf("OnSkip called %d times on a clean source, want 0", calls)
	}
	if !bytes.Equal(readAll(t, dstStrict), readAll(t, dstResync)) {
		t.Error("Resync output differs from strict output on a clean source")
	}
}

// TestReindexResync_RefusesMostlyDamaged wipes most of the payload: the
// resync reindex must refuse rather than pass a stub off as a repair, and a
// file with no surviving cluster at all must be refused with its own message.
func TestReindexResync_RefusesMostlyDamaged(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	tracks := []mkv.Track{videoTrack(1)}
	blockSets := make([][]mkv.Block, 0, 6)
	for i := 0; i < 6; i++ {
		blockSets = append(blockSets, []mkv.Block{
			{TrackNumber: 1, Timecode: int64(i * 1000), Keyframe: true, Data: bytes.Repeat([]byte{0xAB}, 300)},
		})
	}
	src := buildMultiClusterMKV(t, dir, "big.mkv", tracks, blockSets, 6000)
	data := readAll(t, src)

	magic := []byte{0x1F, 0x43, 0xB6, 0x75}
	offsets := findAll(data, magic)
	if len(offsets) < 6 {
		t.Fatalf("fixture has only %d clusters, need >=6", len(offsets))
	}

	// Wipe clusters 1-5 of 6: far more than half of the walked payload.
	mostly := append([]byte(nil), data...)
	for i := offsets[0]; i < offsets[5]; i++ {
		mostly[i] = 0
	}
	mostlyPath := filepath.Join(dir, "mostly.corrupt.mkv")
	writeAll(t, mostlyPath, mostly)
	err := Reindex(ctx, mostlyPath, filepath.Join(dir, "mostly.out.mkv"), mkv.Options{Resync: true})
	if err == nil {
		t.Fatal("expected the mostly-damaged file to be refused")
	}
	if !strings.Contains(err.Error(), "refusing to repair") {
		t.Errorf("expected the drop-cap refusal, got: %v", err)
	}

	// Wipe every cluster and the tail: nothing survives.
	all := append([]byte(nil), data...)
	for i := offsets[0]; i < int64(len(all)); i++ {
		all[i] = 0
	}
	allPath := filepath.Join(dir, "all.corrupt.mkv")
	writeAll(t, allPath, all)
	err = Reindex(ctx, allPath, filepath.Join(dir, "all.out.mkv"), mkv.Options{Resync: true})
	if err == nil {
		t.Fatal("expected the fully-damaged file to be refused")
	}
	if !strings.Contains(err.Error(), "no cluster survived") {
		t.Errorf("expected the no-surviving-cluster refusal, got: %v", err)
	}
}

// TestReindexResync_CapExhausted mirrors TestSalvage_ResyncCapExceeded on the
// reindex path: garbage wider than the (shrunk) scan window with real data
// beyond it must fail cleanly, not hang and not silently truncate.
func TestReindexResync_CapExhausted(t *testing.T) {
	orig := salvageResyncCap
	salvageResyncCap = 128
	defer func() { salvageResyncCap = orig }()

	ctx := context.Background()
	dir := t.TempDir()
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
	for i := offsets[1]; i < offsets[4]; i++ {
		data[i] = 0
	}
	if offsets[4]-offsets[1] <= salvageResyncCap {
		t.Fatalf("wiped span %d must exceed the shrunk cap %d", offsets[4]-offsets[1], salvageResyncCap)
	}
	corrupted := filepath.Join(dir, "cap.corrupt.mkv")
	writeAll(t, corrupted, data)

	done := make(chan struct{})
	var err error
	go func() {
		err = Reindex(ctx, corrupted, filepath.Join(dir, "cap.out.mkv"), mkv.Options{Resync: true})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Reindex with Resync hung past the scan cap")
	}
	if err == nil {
		t.Fatal("expected an error when the resync scan cap is exceeded")
	}
}

// TestReindexReplace_Resync drives the option through ReindexReplace: the
// corrupted original is atomically replaced by the repaired copy and the
// dropped span is reported.
func TestReindexReplace_Resync(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := salvageFixture(t, dir, "replace.mkv", 8)
	data := readAll(t, src)

	magic := []byte{0x1F, 0x43, 0xB6, 0x75}
	offsets := findAll(data, magic)
	if len(offsets) < 5 {
		t.Fatalf("fixture has only %d clusters, need >=5", len(offsets))
	}
	junk := bytes.Repeat([]byte{0x00, 0xFF, 0x51}, 33)
	at := offsets[3]
	target := filepath.Join(dir, "replace.corrupt.mkv")
	writeAll(t, target, spliceAt(data, at, junk))

	var skipped []mkv.DamagedRange
	err := ReindexReplace(ctx, target, mkv.Options{
		Resync: true,
		OnSkip: func(r mkv.DamagedRange) { skipped = append(skipped, r) },
	})
	if err != nil {
		t.Fatalf("ReindexReplace with Resync: %v", err)
	}
	if len(skipped) != 1 {
		t.Fatalf("expected exactly 1 skipped range, got %d", len(skipped))
	}
	blockReaderIteratesCleanly(t, target)
	validateErrorFree(t, target)
}

// TestReindexInPlace_RefusesResync: the in-place patch cannot drop bytes from
// the file, so the option must be refused up front and the file untouched.
func TestReindexInPlace_RefusesResync(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := salvageFixture(t, dir, "inplace.mkv", 4)
	before := readAll(t, src)

	err := ReindexInPlace(ctx, src, mkv.Options{Resync: true})
	if err == nil {
		t.Fatal("ReindexInPlace should refuse Options.Resync")
	}
	if !strings.Contains(err.Error(), "Reindex or ReindexReplace") {
		t.Errorf("refusal should point at the copy-based variants, got: %v", err)
	}
	if !bytes.Equal(before, readAll(t, src)) {
		t.Error("file must be untouched after the refusal")
	}
}
