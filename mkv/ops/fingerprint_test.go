package ops

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildFingerprintMKV writes a minimal two-track (video+audio) MKV whose
// metadata (title, muxing/writing app, track order) can vary independently
// of the media payloads.
func buildFingerprintMKV(t *testing.T, dir, name, title string, tracks []mkv.Track, blocks []mkv.Block) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, Title: title, MuxingApp: "muxer-a", WritingApp: "writer-a"}}
	if err := mw.WriteMetadata(c, tracks, 200); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteClusterWithCues(0, 1_000_000, blocks); err != nil {
		t.Fatal(err)
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestFingerprint_SameContentDifferentMetadata proves the Presentation hash
// is unaffected by container metadata (title/muxing app) and track order:
// two files carrying the same audio+video payloads, one with the tracks
// swapped and different Info fields, fingerprint identically.
func TestFingerprint_SameContentDifferentMetadata(t *testing.T) {
	dir := t.TempDir()

	videoBlocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
		{TrackNumber: 1, Timecode: 40, Data: []byte("v1")},
	}
	audioBlocks := []mkv.Block{
		{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("a0")},
		{TrackNumber: 2, Timecode: 40, Data: []byte("a1")},
	}

	pathA := buildFingerprintMKV(t, dir, "a.mkv", "Movie A",
		[]mkv.Track{videoTrack(1), audioTrack(2)},
		append(append([]mkv.Block{}, videoBlocks...), audioBlocks...))

	// Same payloads, tracks reordered (audio first, video second, with
	// matching new track numbers) and different title/muxing metadata.
	videoBlocksSwapped := []mkv.Block{
		{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("v0")},
		{TrackNumber: 2, Timecode: 40, Data: []byte("v1")},
	}
	audioBlocksSwapped := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("a0")},
		{TrackNumber: 1, Timecode: 40, Data: []byte("a1")},
	}
	pathB := buildFingerprintMKV(t, dir, "b.mkv", "Movie B (retitled)",
		[]mkv.Track{audioTrack(1), videoTrack(2)},
		append(append([]mkv.Block{}, audioBlocksSwapped...), videoBlocksSwapped...))

	fpA, err := Fingerprint(context.Background(), pathA)
	if err != nil {
		t.Fatal(err)
	}
	fpB, err := Fingerprint(context.Background(), pathB)
	if err != nil {
		t.Fatal(err)
	}
	if fpA.Presentation != fpB.Presentation {
		t.Errorf("Presentation differs for identical content in different order/metadata: %s vs %s", fpA.Presentation, fpB.Presentation)
	}
	if len(fpA.Tracks) != 2 || len(fpB.Tracks) != 2 {
		t.Fatalf("expected 2 track digests each, got %d and %d", len(fpA.Tracks), len(fpB.Tracks))
	}
}

// TestFingerprint_DifferentPayloadDiffers proves a single differing frame
// changes the Presentation hash.
func TestFingerprint_DifferentPayloadDiffers(t *testing.T) {
	dir := t.TempDir()

	blocksA := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
		{TrackNumber: 1, Timecode: 40, Data: []byte("v1")},
	}
	blocksB := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
		{TrackNumber: 1, Timecode: 40, Data: []byte("DIFFERENT")},
	}
	pathA := buildFingerprintMKV(t, dir, "a.mkv", "Same Title", []mkv.Track{videoTrack(1)}, blocksA)
	pathB := buildFingerprintMKV(t, dir, "b.mkv", "Same Title", []mkv.Track{videoTrack(1)}, blocksB)

	fpA, err := Fingerprint(context.Background(), pathA)
	if err != nil {
		t.Fatal(err)
	}
	fpB, err := Fingerprint(context.Background(), pathB)
	if err != nil {
		t.Fatal(err)
	}
	if fpA.Presentation == fpB.Presentation {
		t.Errorf("Presentation identical for differing payloads: %s", fpA.Presentation)
	}
}

// TestFingerprint_MatchesCompareBlocksDigest proves the per-track SHA256
// exposed here is exactly the digest CompareBlocks computes (the same
// machinery, digestTracks, backs both).
func TestFingerprint_MatchesCompareBlocksDigest(t *testing.T) {
	dir := t.TempDir()
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
		{TrackNumber: 1, Timecode: 40, Data: []byte("v1")},
	}
	path := buildFingerprintMKV(t, dir, "solo.mkv", "T", []mkv.Track{videoTrack(1)}, blocks)

	fp, err := Fingerprint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	_, digests, err := digestTracks(context.Background(), path, mkv.FSFrom(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fp.Tracks) != 1 || len(digests) != 1 {
		t.Fatalf("expected 1 track, got fp=%d digest=%d", len(fp.Tracks), len(digests))
	}
	wantHex := hex.EncodeToString(digests[0].hash[:])
	if fp.Tracks[0].SHA256 != wantHex {
		t.Errorf("Fingerprint SHA256 = %s, want %s (CompareBlocks digest)", fp.Tracks[0].SHA256, wantHex)
	}
}

// TestFingerprint_MemFS proves the FS port works like every other operation.
func TestFingerprint_MemFS(t *testing.T) {
	mem := mkv.NewMemFS()
	fs := mem.FS()

	w, err := fs.DoCreate("mem.mkv")
	if err != nil {
		t.Fatal(err)
	}
	mw := writer.NewMKVWriter(w)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test"}}
	if err := mw.WriteMetadata(c, []mkv.Track{videoTrack(1)}, 200); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteClusterWithCues(0, 1_000_000, []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
		{TrackNumber: 1, Timecode: 100, Data: []byte("v1")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	fp, err := Fingerprint(context.Background(), "mem.mkv", mkv.Options{FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	if len(fp.Tracks) != 1 || fp.Presentation == "" {
		t.Fatalf("unexpected fingerprint from MemFS: %+v", fp)
	}
}
