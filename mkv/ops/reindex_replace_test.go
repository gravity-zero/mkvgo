package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestReindexReplace_ReplacesAfterVerify verifies that ReindexReplace leaves
// the rebuilt file at the original path, with the temp file and any backup
// gone.
func TestReindexReplace_ReplacesAfterVerify(t *testing.T) {
	dir := t.TempDir()
	cluster1 := []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}}
	cluster2 := []mkv.Block{{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v1")}}
	src := buildMultiClusterMKV(t, dir, "video.mkv", []mkv.Track{videoTrack(1)}, [][]mkv.Block{cluster1, cluster2}, 2000)

	ctx := context.Background()
	if err := ReindexReplace(ctx, src); err != nil {
		t.Fatalf("ReindexReplace: %v", err)
	}

	c, err := reader.Open(ctx, src)
	if err != nil {
		t.Fatalf("open reindexed output: %v", err)
	}
	if len(c.Cues) == 0 {
		t.Fatal("expected cues in reindexed output")
	}
	assertCuesPointToClusters(t, src, c.Cues)

	if _, err := os.Stat(src + ".mkvgo.tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind (stat err=%v)", err)
	}
	if _, err := os.Stat(src + ".bak"); !os.IsNotExist(err) {
		t.Errorf("unexpected backup file (stat err=%v)", err)
	}
}

// TestReindexReplace_KeepBackup verifies that Options.KeepBackup preserves
// the pre-op original byte-for-byte at path+".bak".
func TestReindexReplace_KeepBackup(t *testing.T) {
	dir := t.TempDir()
	cluster1 := []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}}
	src := buildMultiClusterMKV(t, dir, "video.mkv", []mkv.Track{videoTrack(1)}, [][]mkv.Block{cluster1}, 1000)

	origBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	origHash := sha256.Sum256(origBytes)

	ctx := context.Background()
	if err := ReindexReplace(ctx, src, mkv.Options{KeepBackup: true}); err != nil {
		t.Fatalf("ReindexReplace: %v", err)
	}

	backupBytes, err := os.ReadFile(src + ".bak")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if sha256.Sum256(backupBytes) != origHash {
		t.Error("backup does not match the pre-op original")
	}

	c, err := reader.Open(ctx, src)
	if err != nil {
		t.Fatalf("open reindexed output: %v", err)
	}
	if len(c.Cues) == 0 {
		t.Fatal("expected cues in reindexed output")
	}
}

// TestReindexReplace_SourceInvalid verifies that a source which fails
// verification (here: truncated mid-cluster, so the copy itself errors) never
// touches the original and leaves no temp file behind.
func TestReindexReplace_SourceInvalid(t *testing.T) {
	dir := t.TempDir()

	valid := buildMultiClusterMKV(t, dir, "valid.mkv",
		[]mkv.Track{videoTrack(1)},
		[][]mkv.Block{
			{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: bytes.Repeat([]byte("X"), 512)}},
		},
		1000,
	)
	data, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}

	idx := bytes.Index(data, clusterIDBytes)
	if idx < 0 {
		t.Fatal("IDCluster not found in fixture")
	}
	truncAt := idx + 4 + 8 + 10 // header + size + a few body bytes: guaranteed mid-cluster cut
	if truncAt > len(data) {
		t.Fatal("fixture too short to truncate mid-cluster")
	}

	src := filepath.Join(dir, "corrupt.mkv")
	if err := os.WriteFile(src, data[:truncAt], 0644); err != nil {
		t.Fatal(err)
	}
	origBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := ReindexReplace(ctx, src); err == nil {
		t.Fatal("expected error for a truncated source, got nil")
	}

	afterBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(origBytes, afterBytes) {
		t.Error("original file was modified despite the reindex failure")
	}
	if _, err := os.Stat(src + ".mkvgo.tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind (stat err=%v)", err)
	}
}

// TestReindexReplace_LeftoverTmpRefused verifies that a pre-existing temp file
// is refused rather than silently overwritten, and that nothing is modified.
func TestReindexReplace_LeftoverTmpRefused(t *testing.T) {
	dir := t.TempDir()
	src := buildMultiClusterMKV(t, dir, "video.mkv",
		[]mkv.Track{videoTrack(1)},
		[][]mkv.Block{{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}}},
		1000,
	)

	tmp := src + ".mkvgo.tmp"
	if err := os.WriteFile(tmp, []byte("leftover"), 0644); err != nil {
		t.Fatal(err)
	}
	origBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	err = ReindexReplace(ctx, src)
	if err == nil {
		t.Fatal("expected error for a leftover temp file, got nil")
	}
	if !strings.Contains(err.Error(), "leftover temporary file") {
		t.Errorf("error = %q, want it to mention the leftover temporary file", err.Error())
	}

	afterBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(origBytes, afterBytes) {
		t.Error("original file was modified")
	}
	tmpBytes, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if string(tmpBytes) != "leftover" {
		t.Error("leftover temp file was modified")
	}
}

// TestReindexReplace_MemFS exercises the full operation on an in-memory FS,
// proving the FS.Rename path (both the plain overwrite and, implicitly, the
// key manipulation ReindexReplace relies on).
func TestReindexReplace_MemFS(t *testing.T) {
	dir := t.TempDir()
	cluster1 := []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}}
	cluster2 := []mkv.Block{{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v1")}}
	realPath := buildMultiClusterMKV(t, dir, "video.mkv", []mkv.Track{videoTrack(1)}, [][]mkv.Block{cluster1, cluster2}, 2000)
	data, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatal(err)
	}

	m := mkv.NewMemFS()
	const path = "video.mkv"
	m.Put(path, data)

	ctx := context.Background()
	if err := ReindexReplace(ctx, path, mkv.Options{FS: m.FS()}); err != nil {
		t.Fatalf("ReindexReplace on MemFS: %v", err)
	}

	c, err := reader.OpenWithFS(ctx, path, m.FS())
	if err != nil {
		t.Fatalf("open reindexed MemFS output: %v", err)
	}
	if len(c.Cues) == 0 {
		t.Fatal("expected cues in reindexed output")
	}

	for _, p := range m.Paths() {
		if strings.HasSuffix(p, ".mkvgo.tmp") {
			t.Errorf("leftover tmp key in MemFS: %s", p)
		}
		if strings.HasSuffix(p, ".bak") {
			t.Errorf("unexpected backup key in MemFS: %s", p)
		}
	}
}

// TestReindex_DeepVerifyPasses verifies that Reindex with Options.DeepVerify
// succeeds on a healthy fixture (full-read Validate plus byte-level payload
// comparison against the source).
func TestReindex_DeepVerifyPasses(t *testing.T) {
	dir := t.TempDir()
	cluster1 := []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}}
	cluster2 := []mkv.Block{{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v1")}}
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, [][]mkv.Block{cluster1, cluster2}, 2000)
	dst := filepath.Join(dir, "dst.mkv")

	ctx := context.Background()
	if err := Reindex(ctx, src, dst, mkv.Options{DeepVerify: true}); err != nil {
		t.Fatalf("Reindex with DeepVerify: %v", err)
	}

	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	if len(c.Cues) == 0 {
		t.Fatal("expected cues in output")
	}
}

// TestReindex_LightVerifyRuns proves the always-on light verify is actually
// wired: it passes against the real cues produced by Reindex, and catches a
// deliberately wrong expectation when called directly.
func TestReindex_LightVerifyRuns(t *testing.T) {
	dir := t.TempDir()
	src := buildMultiClusterMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[][]mkv.Block{{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}}},
		1000,
	)
	dst := filepath.Join(dir, "dst.mkv")

	ctx := context.Background()
	if err := Reindex(ctx, src, dst); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyReindexedCues(ctx, dst, nil, c.Cues, 1_000_000); err != nil {
		t.Errorf("verifyReindexedCues rejected a correct expectation: %v", err)
	}

	wrong := []mkv.CuePoint{{TimeMs: 12345, Track: 1, ClusterPos: 0}}
	if err := verifyReindexedCues(ctx, dst, nil, wrong, 1_000_000); err == nil {
		t.Error("verifyReindexedCues did not catch a mismatched cue expectation")
	}
}
