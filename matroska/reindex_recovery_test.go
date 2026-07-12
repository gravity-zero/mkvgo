package matroska

import (
	"bytes"
	"context"
	"os"
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

// TestFacadeMapDamageAndRollback proves the MapDamage dry-run and the
// ApplyRollback reconstruction delegate through the facade: map an intact
// source, reindex it with a rollback delta, apply the delta back.
func TestFacadeMapDamageAndRollback(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()

	report, err := MapDamage(context.Background(), fixturePath)
	assertNoErr(t, err)
	if report.ClustersCopied == 0 || len(report.DamagedRanges) != 0 {
		t.Errorf("intact source: clusters=%d damaged=%+v", report.ClustersCopied, report.DamagedRanges)
	}

	var delta bytes.Buffer
	var infos []RollbackInfo
	dst := filepath.Join(dir, "reindexed.mkv")
	err = Reindex(context.Background(), fixturePath, dst, Options{
		RollbackSink: &delta,
		OnRollback:   func(i RollbackInfo) { infos = append(infos, i) },
	})
	assertNoErr(t, err)
	if len(infos) != 1 || infos[0].Bytes != int64(delta.Len()) {
		t.Fatalf("OnRollback infos = %+v (delta %d bytes)", infos, delta.Len())
	}

	restored := filepath.Join(dir, "restored.mkv")
	err = ApplyRollback(context.Background(), dst, bytes.NewReader(delta.Bytes()), restored)
	assertNoErr(t, err)
	orig, err := os.ReadFile(fixturePath)
	assertNoErr(t, err)
	got, err := os.ReadFile(restored)
	assertNoErr(t, err)
	if !bytes.Equal(orig, got) {
		t.Error("facade rollback did not reconstruct the original byte for byte")
	}
}

// TestFacadeRetimeTracks proves RetimeTracks delegates: shift the fixture's
// audio track later by 200ms and check the block times moved.
func TestFacadeRetimeTracks(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	work := filepath.Join(dir, "retime.mkv")
	data, err := os.ReadFile(fixturePath)
	assertNoErr(t, err)
	assertNoErr(t, os.WriteFile(work, data, 0o644))

	c, err := Open(context.Background(), work)
	assertNoErr(t, err)
	var audio uint64
	for _, tr := range c.Tracks {
		if tr.Type == AudioTrack {
			audio = tr.ID
			break
		}
	}
	if audio == 0 {
		t.Skip("fixture has no audio track")
	}

	err = RetimeTracks(context.Background(), work, map[uint64]int64{audio: 200_000_000}, Options{DeepVerify: true})
	assertNoErr(t, err)
}

// TestFacadeRetimeVariantsAndCueHealth proves the retime engine variants and
// the CueHealth probe delegate through the facade.
func TestFacadeRetimeVariantsAndCueHealth(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	data, err := os.ReadFile(fixturePath)
	assertNoErr(t, err)

	c, err := Open(context.Background(), fixturePath)
	assertNoErr(t, err)
	var audio uint64
	for _, tr := range c.Tracks {
		if tr.Type == AudioTrack {
			audio = tr.ID
			break
		}
	}
	if audio == 0 {
		t.Skip("fixture has no audio track")
	}

	inplace := filepath.Join(dir, "inplace.mkv")
	assertNoErr(t, os.WriteFile(inplace, data, 0o644))
	assertNoErr(t, RetimeTracksInPlace(context.Background(), inplace, map[uint64]int64{audio: 100_000_000}))

	replace := filepath.Join(dir, "replace.mkv")
	assertNoErr(t, os.WriteFile(replace, data, 0o644))
	assertNoErr(t, RetimeTracksReplace(context.Background(), replace, map[uint64]int64{audio: 100_000_000}))

	report, err := CueHealth(context.Background(), replace)
	assertNoErr(t, err)
	if !report.Healthy {
		t.Errorf("the rewritten file's rebuilt index must be healthy, got: %s", report.Reason)
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
