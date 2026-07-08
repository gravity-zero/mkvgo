package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// --- helpers ---

// copyFile duplicates src's bytes to dst (a fresh, independent file).
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
		t.Fatal(err)
	}
}

// sha256File returns the SHA-256 digest of path's current bytes.
func sha256File(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

// countTopLevelIDs walks path's Segment top level and counts how many
// elements carry the given element ID.
func countTopLevelIDs(t *testing.T, path string, id uint32) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(data)

	ebmlHdr, _, err := ebml.ReadElementHeader(r)
	if err != nil {
		t.Fatalf("parse EBML header: %v", err)
	}
	if _, err := io.CopyN(io.Discard, r, ebmlHdr.Size); err != nil {
		t.Fatalf("skip EBML header: %v", err)
	}
	segHdr, _, err := ebml.ReadElementHeader(r)
	if err != nil {
		t.Fatalf("parse Segment header: %v", err)
	}
	var segEnd int64 = -1
	if segHdr.Size >= 0 {
		cur, _ := r.Seek(0, io.SeekCurrent)
		segEnd = cur + segHdr.Size
	}

	count := 0
	for {
		if segEnd >= 0 {
			cur, _ := r.Seek(0, io.SeekCurrent)
			if cur >= segEnd {
				break
			}
		}
		h, _, err := ebml.ReadElementHeader(r)
		if err != nil {
			break
		}
		if h.ID == id {
			count++
		}
		if h.Size < 0 {
			break
		}
		if _, err := io.CopyN(io.Discard, r, h.Size); err != nil {
			break
		}
	}
	return count
}

// buildNoSlotMKV hand-assembles a minimal, spec-conformant MKV with a
// known-size Segment holding only Info, Tracks and one Cluster: no SeekHead,
// no Void anywhere. Padding on the keyframe payload keeps the Segment body
// large enough that its size VINT is at least 2 bytes wide, so patching the
// Segment size to include the new Cues never itself becomes the limiting
// factor in the "no slot" test.
func buildNoSlotMKV(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)

	var body bytes.Buffer
	info := mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"}
	if err := writer.WriteSegmentInfo(&body, &info, 1000); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteTracks(&body, []mkv.Track{videoTrack(1)}); err != nil {
		t.Fatal(err)
	}
	padded := bytes.Repeat([]byte("K"), 300)
	if err := writer.WriteCluster(&body, 0, 1000000, []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: padded},
	}); err != nil {
		t.Fatal(err)
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := writer.WriteEBMLHeader(f); err != nil {
		t.Fatal(err)
	}
	if _, err := ebml.WriteElementHeader(f, mkv.IDSegment, int64(body.Len())); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(body.Bytes()); err != nil {
		t.Fatal(err)
	}
	return path
}

// inplaceFailNWriter fails exactly its Nth Write call (1-indexed); every
// other Write, Read, Seek, Truncate and Sync call passes through untouched.
type inplaceFailNWriter struct {
	inner mkv.ReadWriteSeekCloser
	n     int
	count int
}

func (w *inplaceFailNWriter) Read(p []byte) (int, error) { return w.inner.Read(p) }
func (w *inplaceFailNWriter) Seek(offset int64, whence int) (int64, error) {
	return w.inner.Seek(offset, whence)
}
func (w *inplaceFailNWriter) Close() error { return w.inner.Close() }
func (w *inplaceFailNWriter) Write(p []byte) (int, error) {
	w.count++
	if w.count == w.n {
		return 0, fmt.Errorf("simulated write failure at call %d", w.n)
	}
	return w.inner.Write(p)
}
func (w *inplaceFailNWriter) Truncate(size int64) error {
	t, ok := w.inner.(interface{ Truncate(size int64) error })
	if !ok {
		return fmt.Errorf("inner handle has no Truncate")
	}
	return t.Truncate(size)
}
func (w *inplaceFailNWriter) Sync() error {
	if s, ok := w.inner.(interface{ Sync() error }); ok {
		return s.Sync()
	}
	return nil
}

// inplaceTruncateFailRWSC always fails Truncate; every other operation
// passes through to the real file, so writes land (and can be Synced), only
// the final journal-removal step cannot complete.
type inplaceTruncateFailRWSC struct {
	inner *os.File
}

func (w *inplaceTruncateFailRWSC) Read(p []byte) (int, error)  { return w.inner.Read(p) }
func (w *inplaceTruncateFailRWSC) Write(p []byte) (int, error) { return w.inner.Write(p) }
func (w *inplaceTruncateFailRWSC) Seek(offset int64, whence int) (int64, error) {
	return w.inner.Seek(offset, whence)
}
func (w *inplaceTruncateFailRWSC) Close() error { return w.inner.Close() }
func (w *inplaceTruncateFailRWSC) Sync() error  { return w.inner.Sync() }
func (w *inplaceTruncateFailRWSC) Truncate(int64) error {
	return fmt.Errorf("simulated truncate failure")
}

// --- Tests ---

// TestReindexInPlace_MatchesReindexCues verifies that ReindexInPlace produces
// the same cue timeline (time + track) as Reindex on the same source, and that
// every cue in the patched file lands on a real Cluster header.
func TestReindexInPlace_MatchesReindexCues(t *testing.T) {
	dir := t.TempDir()
	clusters := [][]mkv.Block{
		{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
			{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("a0")},
		},
		{
			{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v1")},
			{TrackNumber: 2, Timecode: 1000, Keyframe: true, Data: []byte("a1")},
		},
		{
			{TrackNumber: 1, Timecode: 2000, Keyframe: true, Data: []byte("v2")},
			{TrackNumber: 2, Timecode: 2000, Keyframe: true, Data: []byte("a2")},
		},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1), audioTrack(2)}, clusters, 3000)

	ctx := context.Background()

	// Path A: Reindex (copy) to a new file.
	dstReindex := filepath.Join(dir, "reindexed.mkv")
	if err := Reindex(ctx, src, dstReindex); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	// Path B: ReindexInPlace on a working copy.
	inPlace := filepath.Join(dir, "inplace.mkv")
	copyFile(t, src, inPlace)
	if err := ReindexInPlace(ctx, inPlace); err != nil {
		t.Fatalf("ReindexInPlace: %v", err)
	}

	wantC, err := reader.Open(ctx, dstReindex)
	if err != nil {
		t.Fatalf("open reindexed: %v", err)
	}
	gotC, err := reader.Open(ctx, inPlace)
	if err != nil {
		t.Fatalf("open in-place: %v", err)
	}
	if len(gotC.Cues) == 0 {
		t.Fatal("expected cues in in-place output")
	}
	if len(gotC.Cues) != len(wantC.Cues) {
		t.Fatalf("cue count: in-place=%d reindex=%d", len(gotC.Cues), len(wantC.Cues))
	}
	for i := range wantC.Cues {
		if gotC.Cues[i].TimeMs != wantC.Cues[i].TimeMs || gotC.Cues[i].Track != wantC.Cues[i].Track {
			t.Errorf("cue[%d]: in-place={time=%d track=%d} reindex={time=%d track=%d}",
				i, gotC.Cues[i].TimeMs, gotC.Cues[i].Track, wantC.Cues[i].TimeMs, wantC.Cues[i].Track)
		}
	}

	assertCuesPointToClusters(t, inPlace, gotC.Cues)
}

// TestReindexInPlace_ClustersUntouched verifies that ReindexInPlace never
// alters a single cluster payload byte: CompareBlocks against the pristine
// original reports zero diffs and identical per-track block counts.
func TestReindexInPlace_ClustersUntouched(t *testing.T) {
	dir := t.TempDir()
	clusters := [][]mkv.Block{
		{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("keyframe-0")},
			{TrackNumber: 1, Timecode: 33, Data: []byte("delta-1")},
			{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("audio-0")},
		},
		{
			{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("keyframe-1000")},
			{TrackNumber: 2, Timecode: 1000, Keyframe: true, Data: []byte("audio-1000")},
		},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1), audioTrack(2)}, clusters, 2000)

	original := filepath.Join(dir, "original.mkv")
	copyFile(t, src, original)
	work := filepath.Join(dir, "work.mkv")
	copyFile(t, src, work)

	ctx := context.Background()
	if err := ReindexInPlace(ctx, work); err != nil {
		t.Fatalf("ReindexInPlace: %v", err)
	}

	diffs, err := CompareBlocks(ctx, original, work)
	if err != nil {
		t.Fatalf("CompareBlocks: %v", err)
	}
	if len(diffs) != 0 {
		t.Errorf("CompareBlocks found %d diffs, want 0: %+v", len(diffs), diffs)
	}

	origCounts := countBlocksFromFile(t, original, 1000000)
	workCounts := countBlocksFromFile(t, work, 1000000)
	for track, n := range origCounts {
		if workCounts[track] != n {
			t.Errorf("track %d block count: original=%d work=%d", track, n, workCounts[track])
		}
	}
}

// TestReindexInPlace_OldCuesVoided verifies that a source built with a Cues
// element already present (via Finalize) ends up with EXACTLY one Cues
// element after ReindexInPlace: the tail one it writes, not a duplicate.
func TestReindexInPlace_OldCuesVoided(t *testing.T) {
	dir := t.TempDir()
	clusters := [][]mkv.Block{
		{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}},
		{{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v1")}},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, clusters, 2000)

	// Sanity: the source already carries a Cues element (Finalize wrote one).
	if n := countTopLevelIDs(t, src, mkv.IDCues); n != 1 {
		t.Fatalf("source Cues count = %d, want 1 (test premise)", n)
	}

	work := filepath.Join(dir, "work.mkv")
	copyFile(t, src, work)
	ctx := context.Background()
	if err := ReindexInPlace(ctx, work); err != nil {
		t.Fatalf("ReindexInPlace: %v", err)
	}

	if n := countTopLevelIDs(t, work, mkv.IDCues); n != 1 {
		t.Errorf("Cues count after ReindexInPlace = %d, want exactly 1", n)
	}

	c, err := reader.Open(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Cues) == 0 {
		t.Fatal("reader sees no cues after ReindexInPlace")
	}
}

// TestReindexInPlace_CrashMidWriteRollsBack verifies that a write failure at
// any point during the operation either leaves the file untouched or auto-
// rolls it back: file bytes always end up byte-identical to the original.
func TestReindexInPlace_CrashMidWriteRollsBack(t *testing.T) {
	dir := t.TempDir()
	clusters := [][]mkv.Block{
		{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}},
		{{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v1")}},
		{{TrackNumber: 1, Timecode: 2000, Keyframe: true, Data: []byte("v2")}},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, clusters, 3000)
	orig, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	origHash := sha256.Sum256(orig)

	work := filepath.Join(dir, "work.mkv")
	ctx := context.Background()

	succeeded := false
	for n := 1; n <= 200 && !succeeded; n++ {
		if err := os.WriteFile(work, orig, 0644); err != nil {
			t.Fatal(err)
		}
		fs := &mkv.FS{
			OpenFile: func(path string, flag int, perm os.FileMode) (mkv.ReadWriteSeekCloser, error) {
				f, err := os.OpenFile(path, flag, perm)
				if err != nil {
					return nil, err
				}
				return &inplaceFailNWriter{inner: f, n: n}, nil
			},
		}
		opErr := ReindexInPlace(ctx, work, mkv.Options{FS: fs})
		got, rerr := os.ReadFile(work)
		if rerr != nil {
			t.Fatal(rerr)
		}
		gotHash := sha256.Sum256(got)
		if opErr != nil {
			if gotHash != origHash {
				t.Fatalf("N=%d: op failed (%v) but file bytes changed (untouched-or-rolled-back invariant broken)", n, opErr)
			}
			continue
		}
		succeeded = true
	}
	if !succeeded {
		t.Fatal("ReindexInPlace never succeeded within 200 simulated write-failure points")
	}
}

// TestRecoverInPlace_RestoresAfterCrash simulates a crash after the patches
// have landed but before the final truncate could remove the journal: the op
// errors, leaving the file mid-state WITH its journal; RecoverInPlace (via a
// clean FS) must then restore the original bytes.
func TestRecoverInPlace_RestoresAfterCrash(t *testing.T) {
	dir := t.TempDir()
	clusters := [][]mkv.Block{
		{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}},
		{{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v1")}},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, clusters, 2000)
	orig, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	origHash := sha256.Sum256(orig)

	work := filepath.Join(dir, "work.mkv")
	copyFile(t, src, work)

	ctx := context.Background()
	crashFS := &mkv.FS{
		OpenFile: func(path string, flag int, perm os.FileMode) (mkv.ReadWriteSeekCloser, error) {
			f, err := os.OpenFile(path, flag, perm)
			if err != nil {
				return nil, err
			}
			return &inplaceTruncateFailRWSC{inner: f}, nil
		},
	}
	if err := ReindexInPlace(ctx, work, mkv.Options{FS: crashFS}); err == nil {
		t.Fatal("expected an error from the simulated truncate failure")
	}

	// The file must now carry a journal (patches landed, truncate did not).
	got, err := os.ReadFile(work)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) == origHash {
		t.Fatal("test premise broken: file was not modified before the simulated crash")
	}

	recovered, err := RecoverInPlace(ctx, work)
	if err != nil {
		t.Fatalf("RecoverInPlace: %v", err)
	}
	if !recovered {
		t.Fatal("RecoverInPlace reported no journal, expected one left by the simulated crash")
	}
	restored, err := os.ReadFile(work)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(restored) != origHash {
		t.Error("file bytes after RecoverInPlace do not match the original")
	}
}

// TestReindexInPlace_TruncatedRefused verifies that a file truncated mid-
// cluster is refused with an error and left byte-identical.
func TestReindexInPlace_TruncatedRefused(t *testing.T) {
	dir := t.TempDir()
	clusters := [][]mkv.Block{
		{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: bytes.Repeat([]byte("X"), 256)}},
		{{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: bytes.Repeat([]byte("Y"), 256)}},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, clusters, 2000)

	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	idx := bytes.LastIndex(data, clusterIDBytes)
	if idx < 0 {
		t.Fatal("IDCluster not found")
	}
	truncAt := idx + 4 + 8 + 10 // header + a few body bytes of the last cluster
	if truncAt > len(data) {
		t.Fatal("fixture too small to truncate mid-cluster")
	}

	work := filepath.Join(dir, "work.mkv")
	if err := os.WriteFile(work, data[:truncAt], 0644); err != nil {
		t.Fatal(err)
	}
	before := sha256File(t, work)

	if err := ReindexInPlace(context.Background(), work); err == nil {
		t.Fatal("expected an error for a mid-cluster-truncated file")
	}
	if after := sha256File(t, work); after != before {
		t.Error("truncated file bytes changed even though the operation was refused")
	}
}

// TestReindexInPlace_NoSlotRefused verifies that a file with neither a head
// SeekHead nor a head Void is refused with an explicit error, untouched.
func TestReindexInPlace_NoSlotRefused(t *testing.T) {
	dir := t.TempDir()
	work := buildNoSlotMKV(t, dir, "noslot.mkv")
	before := sha256File(t, work)

	err := ReindexInPlace(context.Background(), work)
	if err == nil {
		t.Fatal("expected an error for a file with no SeekHead and no Void slot")
	}
	if after := sha256File(t, work); after != before {
		t.Error("file bytes changed even though the operation was refused")
	}
}

// TestReindexInPlace_AudioOnly verifies the throttled fallback-cue path on an
// audio-only source, mirroring Reindex's own audio-only rule, and that the
// operation's own light verify accepts the result.
func TestReindexInPlace_AudioOnly(t *testing.T) {
	dir := t.TempDir()
	clusters := make([][]mkv.Block, 6)
	for i := range clusters {
		tc := int64(i * 200)
		clusters[i] = []mkv.Block{{TrackNumber: 1, Timecode: tc, Keyframe: false, Data: []byte("audio")}}
	}
	src := buildMultiClusterMKV(t, dir, "audio.mkv", []mkv.Track{audioTrack(1)}, clusters, 1200)

	work := filepath.Join(dir, "work.mkv")
	copyFile(t, src, work)

	ctx := context.Background()
	if err := ReindexInPlace(ctx, work); err != nil {
		t.Fatalf("ReindexInPlace: %v", err)
	}

	c, err := reader.Open(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Cues) == 0 {
		t.Fatal("expected at least 1 cue for audio-only file")
	}
	for i := 1; i < len(c.Cues); i++ {
		if gap := c.Cues[i].TimeMs - c.Cues[i-1].TimeMs; gap < reindexCueMinGapMs {
			t.Errorf("cues[%d] and [%d] are only %dms apart (min=%d)", i-1, i, gap, reindexCueMinGapMs)
		}
	}
}

// TestReindexInPlace_MemFS runs the full operation on an in-memory FS,
// proving the port-only code path (including the Truncate assertion).
func TestReindexInPlace_MemFS(t *testing.T) {
	dir := t.TempDir()
	clusters := [][]mkv.Block{
		{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}},
		{{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v1")}},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, clusters, 2000)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	mem := mkv.NewMemFS()
	const memPath = "work.mkv"
	mem.Put(memPath, data)
	fs := mem.FS()

	ctx := context.Background()
	if err := ReindexInPlace(ctx, memPath, mkv.Options{FS: fs}); err != nil {
		t.Fatalf("ReindexInPlace on MemFS: %v", err)
	}

	c, err := reader.OpenWithFS(ctx, memPath, fs)
	if err != nil {
		t.Fatalf("open MemFS result: %v", err)
	}
	if len(c.Cues) == 0 {
		t.Fatal("expected cues in MemFS result")
	}
}

// TestRecoverInPlace_NoJournal verifies the no-op path: a clean file (no
// journal) yields (false, nil) and is left untouched.
func TestRecoverInPlace_NoJournal(t *testing.T) {
	dir := t.TempDir()
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, [][]mkv.Block{
		{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}},
	}, 1000)
	before := sha256File(t, src)

	recovered, err := RecoverInPlace(context.Background(), src)
	if err != nil {
		t.Fatalf("RecoverInPlace: %v", err)
	}
	if recovered {
		t.Fatal("expected recovered=false for a file with no journal")
	}
	if after := sha256File(t, src); after != before {
		t.Error("file bytes changed even though there was no journal")
	}
}

// TestReindexInPlace_DeepVerify exercises the happy path with DeepVerify set.
func TestReindexInPlace_DeepVerify(t *testing.T) {
	dir := t.TempDir()
	clusters := [][]mkv.Block{
		{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}},
		{{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v1")}},
		{{TrackNumber: 1, Timecode: 2000, Keyframe: true, Data: []byte("v2")}},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, clusters, 3000)
	work := filepath.Join(dir, "work.mkv")
	copyFile(t, src, work)

	ctx := context.Background()
	if err := ReindexInPlace(ctx, work, mkv.Options{DeepVerify: true}); err != nil {
		t.Fatalf("ReindexInPlace with DeepVerify: %v", err)
	}

	c, err := reader.Open(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Cues) == 0 {
		t.Fatal("expected cues after deep-verified ReindexInPlace")
	}
}

// TestReindexInPlace_Idempotent verifies that running ReindexInPlace twice on
// the same file succeeds both times, leaves exactly one Cues element, and
// leaves the cue timeline (time, track and cluster position) unchanged.
func TestReindexInPlace_Idempotent(t *testing.T) {
	dir := t.TempDir()
	clusters := [][]mkv.Block{
		{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}},
		{{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v1")}},
		{{TrackNumber: 1, Timecode: 2000, Keyframe: true, Data: []byte("v2")}},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, clusters, 3000)
	work := filepath.Join(dir, "work.mkv")
	copyFile(t, src, work)

	ctx := context.Background()
	if err := ReindexInPlace(ctx, work); err != nil {
		t.Fatalf("first ReindexInPlace: %v", err)
	}
	first, err := reader.Open(ctx, work)
	if err != nil {
		t.Fatal(err)
	}

	if err := ReindexInPlace(ctx, work); err != nil {
		t.Fatalf("second ReindexInPlace: %v", err)
	}
	if n := countTopLevelIDs(t, work, mkv.IDCues); n != 1 {
		t.Errorf("Cues count after second run = %d, want exactly 1", n)
	}
	second, err := reader.Open(ctx, work)
	if err != nil {
		t.Fatal(err)
	}

	if len(first.Cues) != len(second.Cues) {
		t.Fatalf("cue count changed: first=%d second=%d", len(first.Cues), len(second.Cues))
	}
	for i := range first.Cues {
		if first.Cues[i] != second.Cues[i] {
			t.Errorf("cue[%d] changed: first=%+v second=%+v", i, first.Cues[i], second.Cues[i])
		}
	}
}
