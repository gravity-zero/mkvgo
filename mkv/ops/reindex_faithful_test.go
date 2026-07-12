package ops

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildFaithfulMKV creates a fixture with Tags, Chapters, Attachments, and
// multiple tracks. It returns the path and the raw bytes of each named
// top-level element (captured while writing so we can compare verbatim).
func buildFaithfulMKV(t *testing.T, dir string) (path string) {
	t.Helper()
	path = filepath.Join(dir, "faithful_src.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"},
		Chapters: []mkv.Chapter{
			{ID: 1, Title: "Intro", StartMs: 0, EndMs: 1000},
			{ID: 2, Title: "Main", StartMs: 1000, EndMs: 3000},
		},
		Attachments: []mkv.Attachment{
			{ID: 1, Name: "cover.jpg", MIMEType: "image/jpeg", Data: []byte("JPEGDATA")},
		},
		Tags: []mkv.Tag{
			{
				TargetType: "MOVIE",
				SimpleTags: []mkv.SimpleTag{{Name: "TITLE", Value: "Faithful Test"}},
			},
			{
				TargetType: "EPISODE",
				SimpleTags: []mkv.SimpleTag{{Name: "DESCRIPTION", Value: "verbatim copy test"}},
			},
		},
	}
	tracks := []mkv.Track{
		videoTrack(1),
		audioTrack(2),
		{ID: 3, Type: mkv.AudioTrack, Codec: "dts", Language: "fra", SampleRate: f64(48000), Channels: u8(6)},
	}
	if err := mw.WriteMetadata(c, tracks, 3000); err != nil {
		t.Fatal(err)
	}
	// Two clusters spanning 0..3000ms.
	cluster1 := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v-frame-0")},
		{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("a-eng-0")},
		{TrackNumber: 3, Timecode: 0, Keyframe: true, Data: []byte("a-fra-0")},
	}
	cluster2 := []mkv.Block{
		{TrackNumber: 1, Timecode: 1500, Keyframe: true, Data: []byte("v-frame-1500")},
		{TrackNumber: 2, Timecode: 1500, Keyframe: true, Data: []byte("a-eng-1500")},
		{TrackNumber: 3, Timecode: 1500, Keyframe: true, Data: []byte("a-fra-1500")},
	}
	if err := mw.WriteClusterWithCues(0, 1000000, cluster1); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteClusterWithCues(1500, 1000000, cluster2); err != nil {
		t.Fatal(err)
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// extractRawElement extracts the first occurrence of the element with the given
// EBML ID from an MKV file. Returns the full element bytes (header + body).
func extractRawElement(t *testing.T, path string, targetID uint32) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	r := bytes.NewReader(data)

	// Skip EBML header.
	ebmlHdr, _, err := ebml.ReadElementHeader(r)
	if err != nil {
		t.Fatalf("read EBML header: %v", err)
	}
	if _, err := io.CopyN(io.Discard, r, ebmlHdr.Size); err != nil {
		t.Fatalf("skip EBML header body: %v", err)
	}
	// Skip Segment header.
	_, _, err = ebml.ReadElementHeader(r)
	if err != nil {
		t.Fatalf("read Segment header: %v", err)
	}

	// Walk segment-level elements.
	for {
		startPos := int(int64(len(data)) - int64(r.Len()))
		h, hdrBytes, err := ebml.ReadElementHeader(r)
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("read element header: %v", err)
		}
		if h.ID == uint32(targetID) {
			// Return full element: header bytes + body.
			end := startPos + hdrBytes + int(h.Size)
			if end > len(data) {
				t.Fatalf("element %X extends beyond file", targetID)
			}
			return data[startPos:end]
		}
		if h.Size >= 0 {
			if _, err := io.CopyN(io.Discard, r, h.Size); err != nil {
				t.Fatalf("skip element 0x%X: %v", h.ID, err)
			}
		}
	}
	return nil
}

// TestReindex_FaithfulTagsVerbatim verifies that Tags are byte-identical after
// Reindex (no doubling, no re-serialisation).
func TestReindex_FaithfulTagsVerbatim(t *testing.T) {
	dir := t.TempDir()
	src := buildFaithfulMKV(t, dir)
	dst := filepath.Join(dir, "dst.mkv")

	ctx := context.Background()
	if err := Reindex(ctx, src, dst); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	srcTags := extractRawElement(t, src, mkv.IDTags)
	dstTags := extractRawElement(t, dst, mkv.IDTags)

	if srcTags == nil {
		t.Fatal("no Tags element found in source")
	}
	if dstTags == nil {
		t.Fatal("no Tags element found in output")
	}
	if !bytes.Equal(srcTags, dstTags) {
		t.Errorf("Tags element differs: src=%d bytes, dst=%d bytes", len(srcTags), len(dstTags))
	}

	// Also verify via parsed model: same count, same content.
	srcC, _ := reader.Open(ctx, src)
	dstC, _ := reader.Open(ctx, dst)
	if len(srcC.Tags) != len(dstC.Tags) {
		t.Errorf("tag count: src=%d dst=%d", len(srcC.Tags), len(dstC.Tags))
	}
	for i := range srcC.Tags {
		if i >= len(dstC.Tags) {
			break
		}
		srcT, dstT := srcC.Tags[i], dstC.Tags[i]
		if srcT.TargetType != dstT.TargetType {
			t.Errorf("tag[%d].TargetType: src=%q dst=%q", i, srcT.TargetType, dstT.TargetType)
		}
		if len(srcT.SimpleTags) != len(dstT.SimpleTags) {
			t.Errorf("tag[%d].SimpleTags count: src=%d dst=%d", i, len(srcT.SimpleTags), len(dstT.SimpleTags))
		}
	}
}

// TestReindex_FaithfulChaptersVerbatim verifies that Chapters are byte-identical.
func TestReindex_FaithfulChaptersVerbatim(t *testing.T) {
	dir := t.TempDir()
	src := buildFaithfulMKV(t, dir)
	dst := filepath.Join(dir, "dst.mkv")

	ctx := context.Background()
	if err := Reindex(ctx, src, dst); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	srcCh := extractRawElement(t, src, mkv.IDChapters)
	dstCh := extractRawElement(t, dst, mkv.IDChapters)

	if srcCh == nil {
		t.Fatal("no Chapters element in source")
	}
	if dstCh == nil {
		t.Fatal("no Chapters element in output")
	}
	if !bytes.Equal(srcCh, dstCh) {
		t.Errorf("Chapters element differs: src=%d bytes dst=%d bytes", len(srcCh), len(dstCh))
	}

	srcC, _ := reader.Open(ctx, src)
	dstC, _ := reader.Open(ctx, dst)
	if len(srcC.Chapters) != len(dstC.Chapters) {
		t.Errorf("chapters count: src=%d dst=%d", len(srcC.Chapters), len(dstC.Chapters))
	}
}

// TestReindex_FaithfulAttachmentsVerbatim verifies that Attachments are byte-identical.
func TestReindex_FaithfulAttachmentsVerbatim(t *testing.T) {
	dir := t.TempDir()
	src := buildFaithfulMKV(t, dir)
	dst := filepath.Join(dir, "dst.mkv")

	ctx := context.Background()
	if err := Reindex(ctx, src, dst); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	srcAt := extractRawElement(t, src, mkv.IDAttachments)
	dstAt := extractRawElement(t, dst, mkv.IDAttachments)

	if srcAt == nil {
		t.Fatal("no Attachments element in source")
	}
	if dstAt == nil {
		t.Fatal("no Attachments element in output")
	}
	if !bytes.Equal(srcAt, dstAt) {
		t.Errorf("Attachments element differs: src=%d bytes dst=%d bytes", len(srcAt), len(dstAt))
	}

	srcC, _ := reader.Open(ctx, src)
	dstC, _ := reader.Open(ctx, dst)
	if len(srcC.Attachments) != len(dstC.Attachments) {
		t.Errorf("attachment count: src=%d dst=%d", len(srcC.Attachments), len(dstC.Attachments))
	}
	for i := range srcC.Attachments {
		if i >= len(dstC.Attachments) {
			break
		}
		sa, da := srcC.Attachments[i], dstC.Attachments[i]
		if sa.Name != da.Name {
			t.Errorf("attachment[%d] name: src=%q dst=%q", i, sa.Name, da.Name)
		}
		if !bytes.Equal(sa.Data, da.Data) {
			t.Errorf("attachment[%d] data differs", i)
		}
	}
}

// TestReindex_FaithfulTracksVerbatim verifies that Tracks are byte-identical.
func TestReindex_FaithfulTracksVerbatim(t *testing.T) {
	dir := t.TempDir()
	src := buildFaithfulMKV(t, dir)
	dst := filepath.Join(dir, "dst.mkv")

	ctx := context.Background()
	if err := Reindex(ctx, src, dst); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	srcTr := extractRawElement(t, src, mkv.IDTracks)
	dstTr := extractRawElement(t, dst, mkv.IDTracks)

	if srcTr == nil {
		t.Fatal("no Tracks element in source")
	}
	if dstTr == nil {
		t.Fatal("no Tracks element in output")
	}
	if !bytes.Equal(srcTr, dstTr) {
		t.Errorf("Tracks element differs: src=%d bytes dst=%d bytes", len(srcTr), len(dstTr))
	}

	srcC, _ := reader.Open(ctx, src)
	dstC, _ := reader.Open(ctx, dst)
	if len(srcC.Tracks) != len(dstC.Tracks) {
		t.Errorf("track count: src=%d dst=%d", len(srcC.Tracks), len(dstC.Tracks))
	}
}

// TestReindex_CuesPresent verifies that the output has a valid Cues index and
// each cue points to a Cluster element header.
func TestReindex_CuesPresent(t *testing.T) {
	dir := t.TempDir()
	src := buildFaithfulMKV(t, dir)
	dst := filepath.Join(dir, "dst.mkv")

	ctx := context.Background()
	if err := Reindex(ctx, src, dst); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	if len(c.Cues) == 0 {
		t.Fatal("expected cues in output, got none")
	}
	assertCuesPointToClusters(t, dst, c.Cues)
}

// TestReindex_ClusterPayloadVerbatim verifies that cluster payloads are
// byte-identical to the source (no block re-encoding).
func TestReindex_ClusterPayloadVerbatim(t *testing.T) {
	dir := t.TempDir()
	src := buildFaithfulMKV(t, dir)
	dst := filepath.Join(dir, "dst.mkv")

	ctx := context.Background()
	if err := Reindex(ctx, src, dst); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	srcPayloads := extractClusterBodies(t, src)
	dstPayloads := extractClusterBodies(t, dst)

	if len(srcPayloads) != len(dstPayloads) {
		t.Fatalf("cluster count: src=%d dst=%d", len(srcPayloads), len(dstPayloads))
	}
	for i := range srcPayloads {
		if !bytes.Equal(srcPayloads[i], dstPayloads[i]) {
			t.Errorf("cluster[%d] body differs: src=%d bytes dst=%d bytes", i, len(srcPayloads[i]), len(dstPayloads[i]))
		}
	}
}

// TestReindex_WebMDocTypePreserved builds a fixture with a WebM EBML header
// and verifies the DocType "webm" survives verbatim through Reindex. Since our
// MKV writer always writes "matroska", this would fail if Reindex re-wrote the
// EBML header.
func TestReindex_WebMDocTypePreserved(t *testing.T) {
	dir := t.TempDir()

	// Build a standard "matroska" fixture first, then patch the EBML header to
	// pretend it is WebM. The body is still Matroska-compatible so the reader
	// handles it fine; the test only cares that the DocType bytes are preserved.
	src := buildFaithfulMKV(t, dir)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	// Locate "matroska" inside the EBML header and replace with "webm\x00\x00\x00".
	// The EBML header is the very first element; "matroska" appears within it.
	// We patch the DocType string in-place - same length as "matroska" (8 bytes).
	// "webm" is 4 bytes: pad to same length by extending to "webm\x00\x00\x00\x00"
	// but that changes the size field. Instead, replace with "webmtest" (8 chars).
	const orig = "matroska"
	const patched = "webmtest"
	idx := bytes.Index(data, []byte(orig))
	if idx < 0 {
		t.Fatal("cannot find 'matroska' in EBML header")
	}
	copy(data[idx:], []byte(patched))

	webmSrc := filepath.Join(dir, "src.webm")
	if err := os.WriteFile(webmSrc, data, 0644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst.webm")
	ctx := context.Background()
	if err := Reindex(ctx, webmSrc, dst); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	dstData, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(dstData, []byte(patched)) {
		t.Errorf("DocType %q not found in output; EBML header was re-written", patched)
	}
}

// TestReindex_InvalidSource returns an error (not panic) for a missing file.
func TestReindex_InvalidSource(t *testing.T) {
	ctx := context.Background()
	err := Reindex(ctx, "/nonexistent/file.mkv", "/tmp/out.mkv")
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}

// TestReindex_InvalidDest returns an error (not panic) for an unwritable destination.
func TestReindex_InvalidDest(t *testing.T) {
	dir := t.TempDir()
	src := buildFaithfulMKV(t, dir)

	ctx := context.Background()
	err := Reindex(ctx, src, "/nonexistent/dir/out.mkv")
	if err == nil {
		t.Fatal("expected error for unwritable destination")
	}
}

// TestReindex_TruncatedSource does not panic on truncated input.
func TestReindex_TruncatedSource(t *testing.T) {
	dir := t.TempDir()
	src := buildFaithfulMKV(t, dir)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	trunc := filepath.Join(dir, "trunc.mkv")
	if err := os.WriteFile(trunc, data[:len(data)/2], 0644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	dst := filepath.Join(dir, "dst.mkv")
	// May error; must not panic.
	_ = Reindex(ctx, trunc, dst)
}

// TestReindex_OOMGuard verifies that a crafted cluster size header larger than
// maxReindexClusterSize is rejected without allocating memory.
func TestReindex_OOMGuard(t *testing.T) {
	dir := t.TempDir()
	src := buildFaithfulMKV(t, dir)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	// Find IDCluster and overwrite size with oversize value.
	idx := bytes.Index(data, clusterIDBytes)
	if idx < 0 {
		t.Fatal("IDCluster not found")
	}
	if idx+4+8 > len(data) {
		t.Fatal("file too short to patch")
	}
	// Encode size just above maxReindexClusterSize as 8-byte VINT.
	oversize := []byte{0x01, 0x00, 0x00, 0x00, 0x10, 0x00, 0x00, 0x01}
	copy(data[idx+4:], oversize)
	patched := filepath.Join(dir, "oversized.mkv")
	if err := os.WriteFile(patched, data, 0644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	dst := filepath.Join(dir, "dst.mkv")
	gotErr := Reindex(ctx, patched, dst)
	if gotErr == nil {
		t.Fatal("expected error for oversized cluster, got nil")
	}
}

// TestReindex_ContextCancellation verifies that Reindex respects context cancellation.
func TestReindex_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	// Build a large-ish fixture so the loop runs multiple iterations.
	clusters := make([][]mkv.Block, 10)
	for i := range clusters {
		clusters[i] = []mkv.Block{
			{TrackNumber: 1, Timecode: int64(i * 1000), Keyframe: true, Data: bytes.Repeat([]byte("X"), 4096)},
		}
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, clusters, 10000)
	dst := filepath.Join(dir, "dst.mkv")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := Reindex(ctx, src, dst)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

// TestReindex_WithProgress verifies that the progress callback is called at
// least once per cluster when provided.
func TestReindex_WithProgress(t *testing.T) {
	dir := t.TempDir()
	src := buildFaithfulMKV(t, dir)
	dst := filepath.Join(dir, "dst.mkv")

	var calls int
	ctx := context.Background()
	if err := Reindex(ctx, src, dst, mkv.Options{
		Progress: func(done, total int64) { calls++ },
	}); err != nil {
		t.Fatalf("Reindex with progress: %v", err)
	}
	if calls == 0 {
		t.Error("progress callback was never called")
	}
}

// TestReindex_BlockCountPreserved verifies that all blocks survive the reindex.
func TestReindex_BlockCountPreserved(t *testing.T) {
	dir := t.TempDir()
	src := buildFaithfulMKV(t, dir)
	dst := filepath.Join(dir, "dst.mkv")

	ctx := context.Background()
	if err := Reindex(ctx, src, dst); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	srcCounts := countBlocksFromFile(t, src, 1000000)
	dstCounts := countBlocksFromFile(t, dst, 1000000)

	for trackID, srcN := range srcCounts {
		if dstCounts[trackID] != srcN {
			t.Errorf("track %d: src=%d blocks dst=%d blocks", trackID, srcN, dstCounts[trackID])
		}
	}
}

// --- Benchmark ---

// BenchmarkReindex measures verbatim-copy throughput on a large synthetic fixture.
func BenchmarkReindex_Faithful(b *testing.B) {
	dir := b.TempDir()

	const blockSize = 4 * 1024
	const blocksPerCluster = 10
	const numClusters = 50

	payload := bytes.Repeat([]byte("X"), blockSize)
	clusters := make([][]mkv.Block, numClusters)
	for ci := range clusters {
		clusterMs := int64(ci * 1000)
		blocks := make([]mkv.Block, blocksPerCluster)
		for bi := range blocks {
			blocks[bi] = mkv.Block{
				TrackNumber: 1,
				Timecode:    clusterMs + int64(bi*33),
				Keyframe:    bi == 0,
				Data:        payload,
			}
		}
		clusters[ci] = blocks
	}

	src := filepath.Join(dir, "bench_src.mkv")
	{
		f, _ := os.Create(src)
		mw := writer.NewMKVWriter(f)
		mw.WriteStart()
		c := &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"},
		}
		mw.WriteMetadata(c, []mkv.Track{videoTrack(1)}, int64(numClusters*1000))
		for _, blks := range clusters {
			mw.WriteClusterWithCues(blks[0].Timecode, 1000000, blks)
		}
		mw.Finalize()
		f.Close()
	}

	info, _ := os.Stat(src)
	b.ResetTimer()
	b.SetBytes(info.Size())

	for i := 0; i < b.N; i++ {
		dst := filepath.Join(dir, "bench_dst.mkv")
		if err := Reindex(context.Background(), src, dst); err != nil {
			b.Fatal(err)
		}
		os.Remove(dst)
	}
}
