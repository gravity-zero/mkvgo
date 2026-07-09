package matroska

import (
	"context"
	"path/filepath"
	"testing"
)

// TestFacadeReindexReplace proves ReindexReplace produces a readable,
// reindexed file at the original path.
func TestFacadeReindexReplace(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "copy.mkv")
	copyFile(t, fixturePath, path)

	assertNoErr(t, ReindexReplace(context.Background(), path))

	c, err := Open(context.Background(), path)
	assertNoErr(t, err)
	if len(c.Cues) == 0 {
		t.Error("expected cues in the reindexed output")
	}
}

// TestFacadeReindexInPlace proves ReindexInPlace patches the file itself
// (no copy) and leaves it readable with a cues index.
func TestFacadeReindexInPlace(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "copy.mkv")
	copyFile(t, fixturePath, path)

	assertNoErr(t, ReindexInPlace(context.Background(), path))

	c, err := Open(context.Background(), path)
	assertNoErr(t, err)
	if len(c.Cues) == 0 {
		t.Error("expected cues in the reindexed output")
	}
}

// TestFacadeRecoverInPlaceNoJournal proves RecoverInPlace returns (false,nil)
// on a file that carries no in-file journal (nothing to roll back).
func TestFacadeRecoverInPlaceNoJournal(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "copy.mkv")
	copyFile(t, fixturePath, path)

	recovered, err := RecoverInPlace(context.Background(), path)
	assertNoErr(t, err)
	if recovered {
		t.Error("RecoverInPlace on a clean file should report recovered=false")
	}
}

// TestFacadeSalvage proves Salvage delegates and, on an intact source,
// reports zero damaged ranges.
func TestFacadeSalvage(t *testing.T) {
	requireFixture(t)
	dst := filepath.Join(t.TempDir(), "salvaged.mkv")

	report, err := Salvage(context.Background(), fixturePath, dst)
	assertNoErr(t, err)
	if report.ClustersCopied == 0 {
		t.Error("expected at least one cluster copied")
	}
	if len(report.DamagedRanges) != 0 {
		t.Errorf("intact source reported damaged ranges: %+v", report.DamagedRanges)
	}

	c, err := Open(context.Background(), dst)
	assertNoErr(t, err)
	if len(c.Cues) == 0 {
		t.Error("expected cues in the salvaged output")
	}
}

// TestFacadeContentHashes proves WriteContentHashes + VerifyContentHashes
// round-trip: a freshly hashed file reports no mismatches.
func TestFacadeContentHashes(t *testing.T) {
	requireFixture(t)
	dst := filepath.Join(t.TempDir(), "hashed.mkv")

	assertNoErr(t, WriteContentHashes(context.Background(), fixturePath, dst))

	mismatches, err := VerifyContentHashes(context.Background(), dst)
	assertNoErr(t, err)
	if len(mismatches) != 0 {
		t.Errorf("pristine hashed file reported mismatches: %+v", mismatches)
	}
}

// TestFacadeCompareBlocks proves CompareBlocks of a file against itself
// returns zero diffs.
func TestFacadeCompareBlocks(t *testing.T) {
	requireFixture(t)
	diffs, err := CompareBlocks(context.Background(), fixturePath, fixturePath)
	assertNoErr(t, err)
	if len(diffs) != 0 {
		t.Errorf("CompareBlocks(self, self) = %+v, want empty", diffs)
	}
}

// TestFacadeCompareContainers proves CompareContainers of a container against
// itself returns zero diffs.
func TestFacadeCompareContainers(t *testing.T) {
	c := requireFixture(t)
	diffs := CompareContainers(c, c)
	if len(diffs) != 0 {
		t.Errorf("CompareContainers(self, self) = %+v, want empty", diffs)
	}
}
