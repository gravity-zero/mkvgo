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

// --- helpers ---

// buildMultiClusterMKV builds an MKV with multiple clusters spaced far enough
// apart to guarantee N distinct clusters (one per block set crossing 1000ms).
func buildMultiClusterMKV(t *testing.T, dir, name string, tracks []mkv.Track, blockSets [][]mkv.Block, durationMs int64) string {
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
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"},
	}
	if err := mw.WriteMetadata(c, tracks, durationMs); err != nil {
		t.Fatal(err)
	}
	for _, blocks := range blockSets {
		if len(blocks) == 0 {
			continue
		}
		if err := mw.WriteClusterWithCues(blocks[0].Timecode, 1000000, blocks); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// buildBlockGroupMKV writes a cluster containing a BlockGroup (with optional
// ReferenceBlock) using raw EBML to simulate a real encoder's output.
func buildBlockGroupMKV(t *testing.T, dir, name string, withRef bool) string {
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
	c := &mkv.Container{
		Info:   mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"},
		Tracks: []mkv.Track{videoTrack(1)},
	}
	if err := mw.WriteMetadata(c, c.Tracks, 100); err != nil {
		t.Fatal(err)
	}

	// Build a cluster with a BlockGroup manually.
	// Block payload: track VINT (0x81 = track 1) + relTC (0x00 0x00) + flags + 3 bytes of frame data.
	// flags 0x80 = keyframe (no ReferenceBlock); 0x00 = non-keyframe (withRef path adds a ReferenceBlock).
	flags := byte(0x00)
	if !withRef {
		flags = 0x80
	}
	blockPayload := []byte{0x81, 0x00, 0x00, flags, 0x01, 0x02, 0x03}

	// Build the Block element.
	var blockElem bytes.Buffer
	ebml.WriteElementID(&blockElem, mkv.IDBlock)
	ebml.WriteDataSize(&blockElem, int64(len(blockPayload)))
	blockElem.Write(blockPayload)

	// Build ReferenceBlock if needed.
	var refElem bytes.Buffer
	if withRef {
		// ReferenceBlock = 0xFB, size=1, value=0 (preceding frame)
		ebml.WriteElementID(&refElem, mkv.IDReferenceBlock)
		ebml.WriteDataSize(&refElem, 1)
		refElem.WriteByte(0x00)
	}

	// Build BlockGroup.
	bgBody := append(blockElem.Bytes(), refElem.Bytes()...)
	var bgElem bytes.Buffer
	ebml.WriteElementID(&bgElem, mkv.IDBlockGroup)
	ebml.WriteDataSize(&bgElem, int64(len(bgBody)))
	bgElem.Write(bgBody)

	// Timestamp element.
	var tsElem bytes.Buffer
	ebml.WriteElementID(&tsElem, mkv.IDTimestamp)
	ebml.WriteDataSize(&tsElem, 1)
	tsElem.WriteByte(0x00) // timestamp = 0 raw units

	// Cluster body.
	clusterBody := append(tsElem.Bytes(), bgElem.Bytes()...)
	ebml.WriteElementID(mw.W, mkv.IDCluster)
	ebml.WriteDataSize(mw.W, int64(len(clusterBody)))
	mw.W.(io.Writer).Write(clusterBody)

	// We wrote the cluster manually; also add cue so Finalize works.
	mw.Cues = append(mw.Cues, mkv.CuePoint{
		TimeMs:     0,
		Track:      1,
		ClusterPos: mw.RelPos() - int64(ebml.ElementIDLen(mkv.IDCluster)+ebml.DataSizeLen(int64(len(clusterBody)))+len(clusterBody)),
	})

	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// clusterIDBytes is the 4-byte big-endian encoding of IDCluster (0x1F43B675).
var clusterIDBytes = []byte{0x1F, 0x43, 0xB6, 0x75}

// assertCuesPointToClusters reads the output file and verifies that each
// CuePoint.ClusterPos (relative to SegDataStart) lands on a Cluster element ID.
func assertCuesPointToClusters(t *testing.T, path string, cues []mkv.CuePoint) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}

	// Find SegDataStart by parsing the file header sequentially.
	// We keep a running position counter rather than trusting seek arithmetic.
	r := bytes.NewReader(data)

	// Skip the EBML header element (ID + size + body).
	ebmlHdr, n1, err := ebml.ReadElementHeader(r)
	if err != nil {
		t.Fatalf("parse EBML header: %v", err)
	}
	if _, err := io.CopyN(io.Discard, r, ebmlHdr.Size); err != nil {
		t.Fatalf("skip EBML header body: %v", err)
	}
	// Position now = n1 + ebmlHdr.Size = start of Segment element.

	// Read Segment ID + size (variable width).
	_, n2, err := ebml.ReadElementHeader(r)
	if err != nil {
		t.Fatalf("parse Segment header: %v", err)
	}
	// SegDataStart offset in file = n1 + ebmlHdr.Size + n2.
	segDataStart := int64(n1) + ebmlHdr.Size + int64(n2)

	// Verify each cue.
	for i, cue := range cues {
		fileOff := segDataStart + cue.ClusterPos
		if fileOff < 0 || int(fileOff)+4 > len(data) {
			t.Errorf("cue[%d]: ClusterPos=%d → absolute offset %d out of bounds (file=%d bytes)",
				i, cue.ClusterPos, fileOff, len(data))
			continue
		}
		got := data[fileOff : fileOff+4]
		if !bytes.Equal(got, clusterIDBytes) {
			t.Errorf("cue[%d]: ClusterPos=%d → got bytes %02x %02x %02x %02x, want IDCluster %02x %02x %02x %02x",
				i, cue.ClusterPos, got[0], got[1], got[2], got[3],
				clusterIDBytes[0], clusterIDBytes[1], clusterIDBytes[2], clusterIDBytes[3])
		}
	}
}

// countBlocksFromFile counts blocks per track using the BlockReader.
func countBlocksFromFile(t *testing.T, path string, timecodeScale int64) map[uint64]int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, timecodeScale)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[uint64]int{}
	for {
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("BlockReader.Next: %v", err)
		}
		counts[blk.TrackNumber]++
	}
	return counts
}

// --- Tests ---

// TestReindex_SimpleBlock verifies the fast path on a video+audio MKV with
// SimpleBlocks, multiple clusters, correct cues, and verbatim payload copy.
func TestReindex_SimpleBlock(t *testing.T) {
	dir := t.TempDir()

	// 3 clusters: one per 1000ms boundary crossing.
	cluster1 := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
		{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("a0")},
	}
	cluster2 := []mkv.Block{
		{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v1")},
		{TrackNumber: 2, Timecode: 1000, Keyframe: true, Data: []byte("a1")},
	}
	cluster3 := []mkv.Block{
		{TrackNumber: 1, Timecode: 2000, Keyframe: true, Data: []byte("v2")},
		{TrackNumber: 2, Timecode: 2000, Keyframe: true, Data: []byte("a2")},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1), audioTrack(2)},
		[][]mkv.Block{cluster1, cluster2, cluster3},
		3000,
	)
	dst := filepath.Join(dir, "dst.mkv")

	ctx := context.Background()
	if err := EditMetadata(ctx, src, dst, func(c *mkv.Container) {
		c.Info.Title = "Reindexed"
	}); err != nil {
		t.Fatal(err)
	}

	// Parse output.
	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}

	// Property 1: Cues are non-empty.
	if len(c.Cues) == 0 {
		t.Fatal("expected cues in output, got none")
	}

	// Property 2: Each ClusterPos lands on a Cluster element header.
	assertCuesPointToClusters(t, dst, c.Cues)

	// Property 4: Tracks preserved.
	if len(c.Tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(c.Tracks))
	}
	if c.Info.Title != "Reindexed" {
		t.Fatalf("expected title 'Reindexed', got %q", c.Info.Title)
	}

	// Property 3: Same block count as source.
	srcCounts := countBlocksFromFile(t, src, 1000000)
	dstCounts := countBlocksFromFile(t, dst, 1000000)
	if srcCounts[1] != dstCounts[1] {
		t.Errorf("video block count: src=%d dst=%d", srcCounts[1], dstCounts[1])
	}
	if srcCounts[2] != dstCounts[2] {
		t.Errorf("audio block count: src=%d dst=%d", srcCounts[2], dstCounts[2])
	}
}

// TestReindex_PayloadVerbatim verifies that the cluster payload bytes in the
// output are byte-identical to the source clusters.
func TestReindex_PayloadVerbatim(t *testing.T) {
	dir := t.TempDir()

	cluster1 := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("payload-kf-0")},
		{TrackNumber: 1, Timecode: 33, Data: []byte("payload-1")},
	}
	cluster2 := []mkv.Block{
		{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("payload-kf-1000")},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[][]mkv.Block{cluster1, cluster2},
		2000,
	)
	dst := filepath.Join(dir, "dst.mkv")
	ctx := context.Background()
	if err := EditMetadata(ctx, src, dst, func(c *mkv.Container) {}); err != nil {
		t.Fatal(err)
	}

	// Extract cluster payloads from source and destination and compare.
	srcPayloads := extractClusterBodies(t, src)
	dstPayloads := extractClusterBodies(t, dst)

	if len(srcPayloads) != len(dstPayloads) {
		t.Fatalf("cluster count: src=%d dst=%d", len(srcPayloads), len(dstPayloads))
	}
	for i := range srcPayloads {
		if !bytes.Equal(srcPayloads[i], dstPayloads[i]) {
			t.Errorf("cluster[%d] body differs: src=%d bytes, dst=%d bytes", i, len(srcPayloads[i]), len(dstPayloads[i]))
		}
	}
}

// extractClusterBodies reads all cluster bodies from an MKV file.
func extractClusterBodies(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Skip EBML header.
	ebmlHdr, _, err := ebml.ReadElementHeader(f)
	if err != nil {
		t.Fatal(err)
	}
	f.Seek(ebmlHdr.Size, io.SeekCurrent)

	// Skip Segment header.
	_, _, err = ebml.ReadElementHeader(f)
	if err != nil {
		t.Fatal(err)
	}

	var bodies [][]byte
	for {
		h, _, err := ebml.ReadElementHeader(f)
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		if h.ID == mkv.IDCluster && h.Size >= 0 {
			body := make([]byte, h.Size)
			if _, err := io.ReadFull(f, body); err != nil {
				t.Fatal(err)
			}
			bodies = append(bodies, body)
		} else if h.Size >= 0 {
			f.Seek(h.Size, io.SeekCurrent)
		}
	}
	return bodies
}

// TestReindex_AudioOnly verifies the audio-only cue throttle (≤1 cue per 500ms).
func TestReindex_AudioOnly(t *testing.T) {
	dir := t.TempDir()

	// Build 6 clusters at 0, 200, 400, 600, 800, 1000ms - audio only.
	// Cues should be emitted at most at 0ms and 600ms (500ms apart).
	clusters := make([][]mkv.Block, 6)
	for i := range clusters {
		tc := int64(i * 200)
		clusters[i] = []mkv.Block{
			{TrackNumber: 1, Timecode: tc, Keyframe: false, Data: []byte("audio")},
		}
	}
	src := buildMultiClusterMKV(t, dir, "audio.mkv",
		[]mkv.Track{audioTrack(1)},
		clusters,
		1200,
	)
	dst := filepath.Join(dir, "dst.mkv")
	ctx := context.Background()
	if err := EditMetadata(ctx, src, dst, func(c *mkv.Container) {}); err != nil {
		t.Fatal(err)
	}

	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}

	// Must have at least 1 cue.
	if len(c.Cues) == 0 {
		t.Fatal("expected at least 1 cue for audio-only file")
	}

	// Verify throttle: no two consecutive cues are < 500ms apart.
	for i := 1; i < len(c.Cues); i++ {
		prev := c.Cues[i-1]
		cur := c.Cues[i]
		// CuePoint.TimeMs as stored by WriteCues / read back by parseCuePoint:
		// parseCuePoint stores the raw IDCueTime value (timecode units).
		// With timecodeScale=1000000 and TimeMs in ms, the raw unit equals TimeMs.
		// So (cur.TimeMs - prev.TimeMs) is in raw units = milliseconds for scale=1000000.
		gapMs := cur.TimeMs - prev.TimeMs
		if gapMs < reindexCueMinGapMs {
			t.Errorf("cues[%d] and [%d] are only %dms apart (min=%d)", i-1, i, gapMs, reindexCueMinGapMs)
		}
	}

	assertCuesPointToClusters(t, dst, c.Cues)
}

// TestReindex_BlockGroup_Keyframe verifies that a BlockGroup without ReferenceBlock
// is correctly detected as a keyframe and receives a cue.
func TestReindex_BlockGroup_Keyframe(t *testing.T) {
	dir := t.TempDir()
	// Build a cluster with a keyframe BlockGroup (no ReferenceBlock).
	src := buildBlockGroupMKV(t, dir, "bg_kf.mkv", false /* withRef=false → keyframe */)
	dst := filepath.Join(dir, "dst.mkv")

	ctx := context.Background()
	if err := EditMetadata(ctx, src, dst, func(c *mkv.Container) {}); err != nil {
		t.Fatal(err)
	}

	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Cues) == 0 {
		t.Fatal("expected cue for keyframe BlockGroup")
	}
	assertCuesPointToClusters(t, dst, c.Cues)
}

// TestReindex_BlockGroup_WithRef verifies that a BlockGroup with a ReferenceBlock
// is NOT treated as a keyframe: a keyframeless cluster of a VIDEO file receives no
// cue at all (a Cues index holds only seekable video keyframes), rather than a
// mid-GOP fallback cue that would later fail Validate.
func TestReindex_BlockGroup_WithRef(t *testing.T) {
	dir := t.TempDir()
	// BlockGroup with ReferenceBlock → not a keyframe.
	src := buildBlockGroupMKV(t, dir, "bg_ref.mkv", true /* withRef=true */)
	dst := filepath.Join(dir, "dst.mkv")

	ctx := context.Background()
	if err := EditMetadata(ctx, src, dst, func(c *mkv.Container) {}); err != nil {
		t.Fatal(err)
	}

	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Cues) != 0 {
		t.Fatalf("a keyframeless cluster of a video file must get no cue, got %d", len(c.Cues))
	}
}

// TestReindex_TimeBasedClusters_NoAudioCue is the Avatar-class regression: a
// mixed video+audio file cut into time-based clusters (the common muxer layout) has clusters
// with no video keyframe. reindex must NOT emit a fallback cue on the audio there
// - such a cue misdirects a seek and makes the rebuilt file fail its own Validate
// ("seeking lands on audio, not a keyframe"). Every cue must key on the video.
func TestReindex_TimeBasedClusters_NoAudioCue(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "timebased.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info:   mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"},
		Tracks: []mkv.Track{videoTrack(1), audioTrack(2)},
	}
	if err := mw.WriteMetadata(c, c.Tracks, 100); err != nil {
		t.Fatal(err)
	}

	// One SimpleBlock cluster; returns its start position (relative to segment
	// data) so a source cue can be written for Finalize.
	writeCluster := func(tsRaw, track byte, key bool) int64 {
		pos := mw.RelPos()
		flags := byte(0x00)
		if key {
			flags = 0x80
		}
		block := []byte{0x80 | track, 0x00, 0x00, flags, 0x01, 0x02, 0x03}
		var sb bytes.Buffer
		ebml.WriteElementID(&sb, mkv.IDSimpleBlock)
		ebml.WriteDataSize(&sb, int64(len(block)))
		sb.Write(block)
		var ts bytes.Buffer
		ebml.WriteElementID(&ts, mkv.IDTimestamp)
		ebml.WriteDataSize(&ts, 1)
		ts.WriteByte(tsRaw)
		body := append(ts.Bytes(), sb.Bytes()...)
		ebml.WriteElementID(mw.W, mkv.IDCluster)
		ebml.WriteDataSize(mw.W, int64(len(body)))
		mw.W.(io.Writer).Write(body)
		return pos
	}
	p0 := writeCluster(0, 1, true)  // video keyframe → gets a cue
	writeCluster(10, 2, true)       // audio only (no video keyframe) → must get NO cue
	p2 := writeCluster(20, 1, true) // video keyframe → gets a cue
	mw.Cues = append(mw.Cues,
		mkv.CuePoint{TimeMs: 0, Track: 1, ClusterPos: p0},
		mkv.CuePoint{TimeMs: 20, Track: 1, ClusterPos: p2})
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "dst.mkv")
	ctx := context.Background()
	if err := EditMetadata(ctx, src, dst, func(c *mkv.Container) {}); err != nil {
		t.Fatal(err)
	}
	out, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, cue := range out.Cues {
		if cue.Track != 1 {
			t.Errorf("cue on track %d - reindex must cue only video keyframes, never the audio fallback", cue.Track)
		}
	}
	if len(out.Cues) != 2 {
		t.Errorf("cues = %d, want 2 (only the two video-keyframe clusters)", len(out.Cues))
	}
	// The rebuilt file must pass its own Validate - no blocking "lands on audio".
	issues, err := Validate(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, is := range issues {
		if is.Severity == mkv.SeverityError {
			t.Errorf("reindexed file fails its own Validate: %s", is.Message)
		}
	}
}

// TestReindex_UnknownSizeCluster verifies that reindexFastCopy returns
// errUnknownSizeCluster when the first cluster has unknown size. This guard
// is defense-in-depth: in practice, EditMetadata's reader.OpenWithFS call
// will reject streaming files before reaching reindexFastCopy.
func TestReindex_UnknownSizeCluster(t *testing.T) {
	dir := t.TempDir()

	// Build a valid MKV then patch the cluster size to "unknown".
	src := buildMultiClusterMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[][]mkv.Block{
			{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}},
		},
		1000,
	)

	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	// Find IDCluster (4 bytes: 0x1F 0x43 0xB6 0x75) and patch its size field to
	// the unknown-size sentinel: 0x01 0xFF 0xFF 0xFF 0xFF 0xFF 0xFF 0xFF.
	idx := bytes.Index(data, clusterIDBytes)
	if idx < 0 {
		t.Fatal("IDCluster not found in source file")
	}
	if idx+4+8 > len(data) {
		t.Fatal("file too short to patch cluster size")
	}
	copy(data[idx+4:], []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	patched := filepath.Join(dir, "patched.mkv")
	if err := os.WriteFile(patched, data, 0644); err != nil {
		t.Fatal(err)
	}

	// Call reindexFastCopy directly (bypassing reader.OpenWithFS which would
	// also reject the file). We need a valid MKVWriter positioned at the
	// segment-data offset so RelPos() works.
	dstPath := filepath.Join(dir, "dst.mkv")
	dstFile, err := os.Create(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dstFile.Close()

	mw := writer.NewMKVWriter(dstFile)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000},
	}
	if err := mw.WriteMetadata(c, []mkv.Track{videoTrack(1)}, 1000); err != nil {
		t.Fatal(err)
	}

	err = reindexFastCopy(mw, patched, 1000000, nil, nil, 0, nil)
	if err != errUnknownSizeCluster {
		t.Errorf("expected errUnknownSizeCluster, got %v", err)
	}
}

// TestReindex_OversizedCluster verifies that a crafted cluster header claiming a
// size larger than maxReindexClusterSize is rejected without allocating memory.
func TestReindex_OversizedCluster(t *testing.T) {
	dir := t.TempDir()

	src := buildMultiClusterMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[][]mkv.Block{
			{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}},
		},
		1000,
	)

	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	// Locate the Cluster element (IDCluster = 0x1F43B675) and overwrite its
	// 8-byte data-size field with a value just above maxReindexClusterSize.
	// EBML data-size encoding for large values uses the 8-byte form:
	//   0x01 <7 big-endian bytes of actual value>
	// maxReindexClusterSize = 256<<20 = 0x10000000. We write 0x10000001.
	idx := bytes.Index(data, clusterIDBytes)
	if idx < 0 {
		t.Fatal("IDCluster not found in source file")
	}
	if idx+4+8 > len(data) {
		t.Fatal("file too short to patch cluster size")
	}
	// Encode 0x10000001 as VINT: 0x01 00 00 00 10 00 00 01
	oversize := []byte{0x01, 0x00, 0x00, 0x00, 0x10, 0x00, 0x00, 0x01}
	copy(data[idx+4:], oversize)
	patched := filepath.Join(dir, "patched.mkv")
	if err := os.WriteFile(patched, data, 0644); err != nil {
		t.Fatal(err)
	}

	dstPath := filepath.Join(dir, "dst.mkv")
	dstFile, err := os.Create(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dstFile.Close()

	mw := writer.NewMKVWriter(dstFile)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1000000}}
	if err := mw.WriteMetadata(c, []mkv.Track{videoTrack(1)}, 1000); err != nil {
		t.Fatal(err)
	}

	gotErr := reindexFastCopy(mw, patched, 1000000, nil, nil, 0, nil)
	if gotErr == nil {
		t.Fatal("expected error for oversized cluster, got nil")
	}
	// Must NOT be the unknown-size sentinel - it must be the size-limit error.
	if gotErr == errUnknownSizeCluster {
		t.Fatalf("got errUnknownSizeCluster, want size-limit error")
	}
}

// TestReindex_TruncatedMidCluster verifies that a file truncated in the middle
// of a cluster body returns a read error rather than panicking.
func TestReindex_TruncatedMidCluster(t *testing.T) {
	dir := t.TempDir()

	src := buildMultiClusterMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[][]mkv.Block{
			{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: bytes.Repeat([]byte("X"), 512)}},
		},
		1000,
	)

	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	// Find the cluster and keep only the first 10 bytes of its body.
	idx := bytes.Index(data, clusterIDBytes)
	if idx < 0 {
		t.Fatal("IDCluster not found")
	}
	// Cluster header is 4 (ID) + up to 8 (size) bytes; truncate just past the header.
	truncAt := idx + 4 + 8 + 10
	if truncAt > len(data) {
		truncAt = len(data) / 2
	}
	truncated := filepath.Join(dir, "trunc.mkv")
	if err := os.WriteFile(truncated, data[:truncAt], 0644); err != nil {
		t.Fatal(err)
	}

	dstPath := filepath.Join(dir, "dst.mkv")
	dstFile, err := os.Create(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dstFile.Close()

	mw := writer.NewMKVWriter(dstFile)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1000000}}
	if err := mw.WriteMetadata(c, []mkv.Track{videoTrack(1)}, 1000); err != nil {
		t.Fatal(err)
	}

	// Must not panic; an error is expected (unexpected EOF from io.ReadFull).
	gotErr := reindexFastCopy(mw, truncated, 1000000, nil, nil, 0, nil)
	if gotErr == nil {
		t.Fatal("expected error for mid-cluster truncation, got nil")
	}
}

// TestReindex_MultiCluster_CueTimecodes verifies that CuePoint.TimeMs values
// match the actual first-keyframe timecodes from each cluster.
func TestReindex_MultiCluster_CueTimecodes(t *testing.T) {
	dir := t.TempDir()

	cluster1 := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
	}
	cluster2 := []mkv.Block{
		{TrackNumber: 1, Timecode: 2000, Keyframe: true, Data: []byte("v2000")},
	}
	cluster3 := []mkv.Block{
		{TrackNumber: 1, Timecode: 4000, Keyframe: true, Data: []byte("v4000")},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[][]mkv.Block{cluster1, cluster2, cluster3},
		5000,
	)
	dst := filepath.Join(dir, "dst.mkv")
	ctx := context.Background()
	if err := EditMetadata(ctx, src, dst, func(c *mkv.Container) {}); err != nil {
		t.Fatal(err)
	}

	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}

	// The cues should correspond to timecodes 0, 2000, 4000 (in raw timecode
	// units since parseCuePoint stores raw IDCueTime, and with timecodeScale=1e6
	// raw units = ms).
	expectedTimesMs := []int64{0, 2000, 4000}
	if len(c.Cues) != len(expectedTimesMs) {
		t.Fatalf("expected %d cues, got %d: %+v", len(expectedTimesMs), len(c.Cues), c.Cues)
	}
	for i, cp := range c.Cues {
		// CueTime stored by WriteCues: raw = TimeMs * 1e6 / timecodeScale = TimeMs (scale=1e6).
		// parseCuePoint reads that raw value back directly into cp.TimeMs.
		if cp.TimeMs != expectedTimesMs[i] {
			t.Errorf("cue[%d].TimeMs = %d, want %d", i, cp.TimeMs, expectedTimesMs[i])
		}
	}

	assertCuesPointToClusters(t, dst, c.Cues)
}

// TestReindex_Equivalence verifies that EditMetadata with and without a progress
// callback produce semantically equivalent output (same tracks, same block count).
// Since Fix 3, both paths use reindexFastCopy; the progress callback is simply
// called per-cluster. Byte-level equality is expected since both take the fast path.
func TestReindex_Equivalence(t *testing.T) {
	dir := t.TempDir()

	clusters := [][]mkv.Block{
		{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("key")},
			{TrackNumber: 1, Timecode: 100, Data: []byte("delta1")},
			{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("audio0")},
		},
		{
			{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("key2")},
			{TrackNumber: 2, Timecode: 1000, Keyframe: true, Data: []byte("audio1000")},
		},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1), audioTrack(2)},
		clusters,
		2000,
	)

	// Output without progress callback.
	dstFast := filepath.Join(dir, "fast.mkv")
	ctx := context.Background()
	if err := EditMetadata(ctx, src, dstFast, func(c *mkv.Container) {}); err != nil {
		t.Fatal(err)
	}

	// Output with progress callback - also takes the fast path, and must invoke
	// the callback at least once per cluster.
	var progressCalls int
	dstSlow := filepath.Join(dir, "with_progress.mkv")
	if err := EditMetadata(ctx, src, dstSlow, func(c *mkv.Container) {}, mkv.Options{
		Progress: func(_, _ int64) { progressCalls++ },
	}); err != nil {
		t.Fatal(err)
	}
	if progressCalls == 0 {
		t.Error("progress callback was never called in fast path")
	}

	fastC, err := reader.Open(ctx, dstFast)
	if err != nil {
		t.Fatalf("open fast output: %v", err)
	}
	slowC, err := reader.Open(ctx, dstSlow)
	if err != nil {
		t.Fatalf("open with_progress output: %v", err)
	}

	// Same track count.
	if len(fastC.Tracks) != len(slowC.Tracks) {
		t.Errorf("track count: no-progress=%d with-progress=%d", len(fastC.Tracks), len(slowC.Tracks))
	}

	// Same block count per track.
	fastCounts := countBlocksFromFile(t, dstFast, 1000000)
	slowCounts := countBlocksFromFile(t, dstSlow, 1000000)
	for trackID := range slowCounts {
		if fastCounts[trackID] != slowCounts[trackID] {
			t.Errorf("track %d block count: no-progress=%d with-progress=%d", trackID, fastCounts[trackID], slowCounts[trackID])
		}
	}

	// Both have cues.
	if len(fastC.Cues) == 0 {
		t.Error("fast output has no cues")
	}
	if len(slowC.Cues) == 0 {
		t.Error("slow output has no cues")
	}

	// Both cue positions point to cluster headers.
	assertCuesPointToClusters(t, dstFast, fastC.Cues)
	assertCuesPointToClusters(t, dstSlow, slowC.Cues)
}

// TestReindex_Truncated verifies that a truncated cluster body does not panic
// and either errors gracefully or completes with partial cues.
func TestReindex_Truncated(t *testing.T) {
	dir := t.TempDir()

	src := buildMultiClusterMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[][]mkv.Block{
			{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}},
		},
		1000,
	)

	// Truncate the file to half its size.
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(dir, "trunc.mkv")
	if err := os.WriteFile(truncated, data[:len(data)/2], 0644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst.mkv")
	ctx := context.Background()
	// May error; should NOT panic.
	_ = EditMetadata(ctx, truncated, dst, func(c *mkv.Container) {})
}

// TestReindex_InvalidVINT verifies that a cluster body with a corrupt VINT in
// a SimpleBlock header does not panic.
func TestReindex_InvalidVINT(t *testing.T) {
	dir := t.TempDir()

	// Build a valid MKV and corrupt a block's track VINT to be 0x00 (invalid).
	src := buildMultiClusterMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[][]mkv.Block{
			{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}},
		},
		1000,
	)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	// Find IDSimpleBlock (0xA3) and corrupt the byte following it to 0x00.
	for i := 0; i+2 < len(data); i++ {
		if data[i] == 0xA3 {
			// Zero out the track VINT (makes ReadDataSize return error).
			if i+2 < len(data) {
				data[i+2] = 0x00
			}
			break
		}
	}
	corrupt := filepath.Join(dir, "corrupt.mkv")
	if err := os.WriteFile(corrupt, data, 0644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst.mkv")
	ctx := context.Background()
	// Should not panic; error or success both acceptable.
	_ = EditMetadata(ctx, corrupt, dst, func(c *mkv.Container) {})
}

// TestReindex_WebM exercises the fast path on a WebM-style file (VP8 video,
// Vorbis audio). The format is identical at the Matroska layer.
func TestReindex_WebM(t *testing.T) {
	dir := t.TempDir()

	webmVideo := mkv.Track{
		ID: 1, Type: mkv.VideoTrack, Codec: "vp8", Language: "und",
		Width: u32(640), Height: u32(480), CodecPrivate: []byte{0x01},
	}
	webmAudio := mkv.Track{
		ID: 2, Type: mkv.AudioTrack, Codec: "vorbis", Language: "und",
		SampleRate: f64(44100), Channels: u8(2),
	}
	clusters := [][]mkv.Block{
		{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("vp8-frame0")},
			{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("vorbis-frame0")},
		},
		{
			{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("vp8-frame1")},
			{TrackNumber: 2, Timecode: 1000, Keyframe: true, Data: []byte("vorbis-frame1")},
		},
	}
	src := buildMultiClusterMKV(t, dir, "src.webm",
		[]mkv.Track{webmVideo, webmAudio},
		clusters,
		2000,
	)
	dst := filepath.Join(dir, "dst.webm")
	ctx := context.Background()
	if err := EditMetadata(ctx, src, dst, func(c *mkv.Container) {
		c.Info.Title = "WebM reindexed"
	}); err != nil {
		t.Fatal(err)
	}

	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Cues) == 0 {
		t.Fatal("expected cues in WebM output")
	}
	if len(c.Tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(c.Tracks))
	}
	assertCuesPointToClusters(t, dst, c.Cues)
}

// TestReindex_DurationPreserved verifies that DurationMs is preserved through
// the fast path (written into the Info element, not derived from blocks).
func TestReindex_DurationPreserved(t *testing.T) {
	dir := t.TempDir()
	src := buildMultiClusterMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[][]mkv.Block{
			{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")}},
		},
		12345,
	)
	dst := filepath.Join(dir, "dst.mkv")
	ctx := context.Background()
	if err := EditMetadata(ctx, src, dst, func(c *mkv.Container) {}); err != nil {
		t.Fatal(err)
	}

	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if c.DurationMs != 12345 {
		t.Errorf("expected DurationMs=12345, got %d", c.DurationMs)
	}
}

// TestReindex_TagTrackUID_RoundTrip verifies that after EditMetadata (fast path),
// every Tag.TargetID still matches an existing track's UID.
//
// This is the property that was broken before Fix 2: the writer was using t.ID
// (the small track number) as IDTrackUID, while the reader preserved the
// original large 64-bit UID in Tag.TargetID → dangling reference.
func TestReindex_TagTrackUID_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Build a source with an explicit large TrackUID (as real encoders write).
	const bigUID = uint64(0xDEADBEEF_CAFEBABE)
	track := mkv.Track{
		ID: 1, UID: bigUID,
		Type: mkv.VideoTrack, Codec: "h264", Language: "eng",
		Width: u32(1920), Height: u32(1080), CodecPrivate: []byte{0x01},
	}
	// A tag that references the track by its 64-bit UID.
	tag := mkv.Tag{
		TargetType: "MOVIE",
		TargetID:   bigUID,
		SimpleTags: []mkv.SimpleTag{{Name: "TITLE", Value: "Test"}},
	}

	src := filepath.Join(dir, "src.mkv")
	{
		f, err := os.Create(src)
		if err != nil {
			t.Fatal(err)
		}
		mw := writer.NewMKVWriter(f)
		if err := mw.WriteStart(); err != nil {
			t.Fatal(err)
		}
		c := &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"},
			Tags: []mkv.Tag{tag},
		}
		if err := mw.WriteMetadata(c, []mkv.Track{track}, 1000); err != nil {
			t.Fatal(err)
		}
		if err := mw.WriteClusterWithCues(0, 1000000, []mkv.Block{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("kf")},
		}); err != nil {
			t.Fatal(err)
		}
		if err := mw.Finalize(); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	// Verify the source itself is consistent before reindex.
	ctx := context.Background()
	srcC, err := reader.Open(ctx, src)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	if srcC.Tracks[0].UID != bigUID {
		t.Fatalf("src track UID not preserved by reader: got %d, want %d", srcC.Tracks[0].UID, bigUID)
	}

	// Run EditMetadata (takes fast path since no progress).
	dst := filepath.Join(dir, "dst.mkv")
	if err := EditMetadata(ctx, src, dst, func(c *mkv.Container) {
		c.Info.Title = "Tagged"
	}); err != nil {
		t.Fatal(err)
	}

	dstC, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}

	// Build a set of valid UIDs from the output tracks.
	validUIDs := make(map[uint64]bool, len(dstC.Tracks))
	for _, tr := range dstC.Tracks {
		uid := tr.UID
		if uid == 0 {
			uid = tr.ID
		}
		validUIDs[uid] = true
	}

	// Assert every tag with a non-zero TargetID references a real track UID.
	for i, tg := range dstC.Tags {
		if tg.TargetID == 0 {
			continue
		}
		if !validUIDs[tg.TargetID] {
			t.Errorf("Tag[%d].TargetID=%d does not match any track UID in output (valid: %v)",
				i, tg.TargetID, validUIDs)
		}
	}

	// Specifically: the bigUID tag must survive.
	found := false
	for _, tg := range dstC.Tags {
		if tg.TargetID == bigUID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no tag with TargetID=%d found in output; tags: %+v", bigUID, dstC.Tags)
	}
}

// --- Benchmarks ---

// BenchmarkReindex measures the throughput of the fast path on a large synthetic
// fixture (~N MB of cluster data).
func BenchmarkReindex(b *testing.B) {
	dir := b.TempDir()

	// Build a file with many clusters. Each block has a 4KB payload.
	const blockSize = 4 * 1024
	const blocksPerCluster = 10
	const numClusters = 50 // ~50 * 10 * 4KB = ~2MB

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

	info, _ := os.Stat(src)
	totalBytes := info.Size()

	b.ResetTimer()
	b.SetBytes(totalBytes)

	for i := 0; i < b.N; i++ {
		dst := filepath.Join(dir, "bench_dst.mkv")
		ctx := context.Background()
		if err := EditMetadata(ctx, src, dst, func(c *mkv.Container) {}); err != nil {
			b.Fatal(err)
		}
		os.Remove(dst)
	}
}

// BenchmarkReindexWithProgress measures the fast path with a progress callback.
func BenchmarkReindexWithProgress(b *testing.B) {
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

	info, _ := os.Stat(src)
	totalBytes := info.Size()

	b.ResetTimer()
	b.SetBytes(totalBytes)

	for i := 0; i < b.N; i++ {
		dst := filepath.Join(dir, "bench_dst.mkv")
		ctx := context.Background()
		if err := EditMetadata(ctx, src, dst, func(c *mkv.Container) {}, mkv.Options{
			Progress: func(_, _ int64) {},
		}); err != nil {
			b.Fatal(err)
		}
		os.Remove(dst)
	}
}
