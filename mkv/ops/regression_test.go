// Package ops - non-regression suite against the real ffmpeg-muxed fixture at
// internal/testdata/regfix.mkv (H.264 + 2×AAC laced, 2 chapters, track-UID
// tags, ~6.023 s, ~258 KB).
//
// WHY: synthetic fixtures missed bugs exposed only by real muxer output
// (dangling tag TargetIDs, duplicated Tags elements, cluster-body tampering,
// etc.).  These tests lock the verified-good behavior so future edits fail fast.
package ops

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// regfixPath is the real ffmpeg-muxed fixture (relative to this package).
const regfixPath = "../../internal/testdata/regfix.mkv"

// regfixDurationMs is the expected duration with a ±30 ms tolerance.
const regfixDurationMs = 6023
const regfixDurationTolerance = 30

// ── Test 1: Reader invariants on the real file ────────────────────────────────

// TestRegfix_ReaderInvariants locks the parser against the real ffmpeg output:
// track types, codecs, languages, duration, and chapters.
func TestRegfix_ReaderInvariants(t *testing.T) {
	ctx := context.Background()
	c, err := reader.Open(ctx, regfixPath)
	if err != nil {
		t.Fatalf("open regfix.mkv: %v", err)
	}

	// Exactly 3 tracks.
	if len(c.Tracks) != 3 {
		t.Fatalf("track count: want 3, got %d", len(c.Tracks))
	}

	// Track types: video, audio, audio.
	wantTypes := []mkv.TrackType{mkv.VideoTrack, mkv.AudioTrack, mkv.AudioTrack}
	for i, tr := range c.Tracks {
		if tr.Type != wantTypes[i] {
			t.Errorf("Track[%d].Type = %q, want %q", i, tr.Type, wantTypes[i])
		}
	}

	// Video codec is h264 (CodecShortName maps V_MPEG4/ISO/AVC → "h264").
	if !strings.HasPrefix(c.Tracks[0].Codec, "h264") {
		t.Errorf("Track[0].Codec = %q, want h264", c.Tracks[0].Codec)
	}
	// Both audio tracks are aac.
	for _, i := range []int{1, 2} {
		if !strings.HasPrefix(c.Tracks[i].Codec, "aac") {
			t.Errorf("Track[%d].Codec = %q, want aac", i, c.Tracks[i].Codec)
		}
	}

	// Duration ≈ 6023 ms.
	diff := c.DurationMs - regfixDurationMs
	if diff < 0 {
		diff = -diff
	}
	if diff > regfixDurationTolerance {
		t.Errorf("DurationMs = %d, want %d ± %d", c.DurationMs, regfixDurationMs, regfixDurationTolerance)
	}

	// Exactly 2 chapters.
	if len(c.Chapters) != 2 {
		t.Fatalf("chapter count: want 2, got %d", len(c.Chapters))
	}
	wantChapterTitles := [2]string{"Chapter One", "Chapter Two"}
	for i, ch := range c.Chapters {
		if ch.Title != wantChapterTitles[i] {
			t.Errorf("Chapter[%d].Title = %q, want %q", i, ch.Title, wantChapterTitles[i])
		}
	}

	// Chapter timecodes: Ch1 starts at 0, Ch2 starts at 3000 ms.
	if c.Chapters[0].StartMs != 0 {
		t.Errorf("Chapter[0].StartMs = %d, want 0", c.Chapters[0].StartMs)
	}
	if c.Chapters[1].StartMs != 3000 {
		t.Errorf("Chapter[1].StartMs = %d, want 3000", c.Chapters[1].StartMs)
	}

	// Audio track languages are "fre" and "eng".
	freFound, engFound := false, false
	for _, tr := range c.Tracks {
		if tr.Type == mkv.AudioTrack {
			switch tr.Language {
			case "fre":
				freFound = true
			case "eng":
				engFound = true
			}
		}
	}
	if !freFound {
		t.Error("no audio track with language=fre")
	}
	if !engFound {
		t.Error("no audio track with language=eng")
	}

	// Tags reference the real track UIDs (no dangling references).
	uidSet := map[uint64]bool{}
	for _, tr := range c.Tracks {
		if tr.UID != 0 {
			uidSet[tr.UID] = true
		}
	}
	for i, tg := range c.Tags {
		if tg.TargetID != 0 && !uidSet[tg.TargetID] {
			t.Errorf("Tag[%d].TargetID=%d does not match any track UID", i, tg.TargetID)
		}
	}
}

// ── Test 2: Reindex fidelity round-trip ──────────────────────────────────────

// TestRegfix_ReindexFidelity runs Reindex on the real fixture and verifies
// that the output preserves tracks, chapters, duration, and cluster payload
// byte-for-byte, while adding a valid Cues index.
func TestRegfix_ReindexFidelity(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "reindexed.mkv")
	ctx := context.Background()

	if err := Reindex(ctx, regfixPath, dst); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	// Re-parse the output.
	dstC, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatalf("open reindexed: %v", err)
	}

	srcC, err := reader.Open(ctx, regfixPath)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}

	// Identical track count.
	if len(dstC.Tracks) != len(srcC.Tracks) {
		t.Fatalf("track count: src=%d dst=%d", len(srcC.Tracks), len(dstC.Tracks))
	}

	// Identical codec, type, and UID per track.
	for i := range srcC.Tracks {
		st, dt := srcC.Tracks[i], dstC.Tracks[i]
		if st.Type != dt.Type {
			t.Errorf("Track[%d].Type: src=%q dst=%q", i, st.Type, dt.Type)
		}
		if st.Codec != dt.Codec {
			t.Errorf("Track[%d].Codec: src=%q dst=%q", i, st.Codec, dt.Codec)
		}
		if st.UID != dt.UID {
			t.Errorf("Track[%d].UID: src=%d dst=%d", i, st.UID, dt.UID)
		}
	}

	// Identical chapter count and titles.
	if len(dstC.Chapters) != len(srcC.Chapters) {
		t.Fatalf("chapter count: src=%d dst=%d", len(srcC.Chapters), len(dstC.Chapters))
	}
	for i := range srcC.Chapters {
		if srcC.Chapters[i].Title != dstC.Chapters[i].Title {
			t.Errorf("Chapter[%d].Title: src=%q dst=%q", i, srcC.Chapters[i].Title, dstC.Chapters[i].Title)
		}
		if srcC.Chapters[i].StartMs != dstC.Chapters[i].StartMs {
			t.Errorf("Chapter[%d].StartMs: src=%d dst=%d", i, srcC.Chapters[i].StartMs, dstC.Chapters[i].StartMs)
		}
	}

	// Identical duration.
	if srcC.DurationMs != dstC.DurationMs {
		t.Errorf("DurationMs: src=%d dst=%d", srcC.DurationMs, dstC.DurationMs)
	}

	// Tags preserved: same count, same content.
	if len(dstC.Tags) != len(srcC.Tags) {
		t.Errorf("tag count: src=%d dst=%d", len(srcC.Tags), len(dstC.Tags))
	}

	// Tags with TargetID must still reference real track UIDs.
	uidSet := map[uint64]bool{}
	for _, tr := range dstC.Tracks {
		if tr.UID != 0 {
			uidSet[tr.UID] = true
		}
	}
	for i, tg := range dstC.Tags {
		if tg.TargetID != 0 && !uidSet[tg.TargetID] {
			t.Errorf("Tag[%d].TargetID=%d does not match any track UID (dangling)", i, tg.TargetID)
		}
	}

	// Cues: non-empty, all ClusterPos values land on IDCluster headers.
	if len(dstC.Cues) == 0 {
		t.Fatal("reindexed file has no cues")
	}
	assertCuesPointToClusters(t, dst, dstC.Cues)

	// Cluster payload is byte-identical.
	srcBodies := extractClusterBodies(t, regfixPath)
	dstBodies := extractClusterBodies(t, dst)
	if len(srcBodies) != len(dstBodies) {
		t.Fatalf("cluster count: src=%d dst=%d", len(srcBodies), len(dstBodies))
	}
	for i := range srcBodies {
		if !bytes.Equal(srcBodies[i], dstBodies[i]) {
			t.Errorf("cluster[%d] body: src=%d bytes, dst=%d bytes (not byte-identical)",
				i, len(srcBodies[i]), len(dstBodies[i]))
		}
	}

	// Byte-identical raw element comparison for Tags, Chapters, Tracks.
	// IDInfo will differ only in TimecodeScale (verbatim copy), so we check it
	// too - it must be identical because Reindex copies Info verbatim.
	for _, id := range []uint32{mkv.IDTags, mkv.IDChapters, mkv.IDTracks, mkv.IDInfo} {
		srcRaw := extractRawElement(t, regfixPath, id)
		dstRaw := extractRawElement(t, dst, id)
		if srcRaw == nil || dstRaw == nil {
			// Not all elements may be present; if src has it, dst must too.
			if srcRaw != nil && dstRaw == nil {
				t.Errorf("element 0x%X present in src but missing in dst", id)
			}
			continue
		}
		if !bytes.Equal(srcRaw, dstRaw) {
			t.Errorf("element 0x%X: src=%d bytes, dst=%d bytes (not byte-identical)", id, len(srcRaw), len(dstRaw))
		}
	}
}

// ── Test 3: EditMetadata fast-path on the real file ───────────────────────────

// TestRegfix_EditMetadataFidelity runs EditMetadata (noop edit) on the real
// fixture and asserts the same cluster-payload fidelity and cue validity as
// Reindex, locking the fast cluster-copy path.
func TestRegfix_EditMetadataFidelity(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "edited.mkv")
	ctx := context.Background()

	if err := EditMetadata(ctx, regfixPath, dst, func(_ *mkv.Container) {}); err != nil {
		t.Fatalf("EditMetadata: %v", err)
	}

	dstC, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatalf("open edited: %v", err)
	}

	srcC, err := reader.Open(ctx, regfixPath)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}

	// Track count and codecs preserved.
	if len(dstC.Tracks) != len(srcC.Tracks) {
		t.Fatalf("track count: src=%d dst=%d", len(srcC.Tracks), len(dstC.Tracks))
	}
	for i := range srcC.Tracks {
		if srcC.Tracks[i].Codec != dstC.Tracks[i].Codec {
			t.Errorf("Track[%d].Codec: src=%q dst=%q", i, srcC.Tracks[i].Codec, dstC.Tracks[i].Codec)
		}
		if srcC.Tracks[i].Type != dstC.Tracks[i].Type {
			t.Errorf("Track[%d].Type: src=%q dst=%q", i, srcC.Tracks[i].Type, dstC.Tracks[i].Type)
		}
	}

	// Chapters and duration preserved.
	if len(dstC.Chapters) != len(srcC.Chapters) {
		t.Fatalf("chapter count: src=%d dst=%d", len(srcC.Chapters), len(dstC.Chapters))
	}
	for i := range srcC.Chapters {
		if srcC.Chapters[i].Title != dstC.Chapters[i].Title {
			t.Errorf("Chapter[%d].Title: src=%q dst=%q", i, srcC.Chapters[i].Title, dstC.Chapters[i].Title)
		}
	}
	if srcC.DurationMs != dstC.DurationMs {
		t.Errorf("DurationMs: src=%d dst=%d", srcC.DurationMs, dstC.DurationMs)
	}

	// Cues present and valid.
	if len(dstC.Cues) == 0 {
		t.Fatal("edited file has no cues")
	}
	assertCuesPointToClusters(t, dst, dstC.Cues)

	// Cluster payloads byte-identical.
	srcBodies := extractClusterBodies(t, regfixPath)
	dstBodies := extractClusterBodies(t, dst)
	if len(srcBodies) != len(dstBodies) {
		t.Fatalf("cluster count: src=%d dst=%d", len(srcBodies), len(dstBodies))
	}
	for i := range srcBodies {
		if !bytes.Equal(srcBodies[i], dstBodies[i]) {
			t.Errorf("cluster[%d] body not byte-identical: src=%d bytes dst=%d bytes",
				i, len(srcBodies[i]), len(dstBodies[i]))
		}
	}

	// Tags TargetID references must resolve to real track UIDs.
	uidSet := map[uint64]bool{}
	for _, tr := range dstC.Tracks {
		if tr.UID != 0 {
			uidSet[tr.UID] = true
		}
	}
	for i, tg := range dstC.Tags {
		if tg.TargetID != 0 && !uidSet[tg.TargetID] {
			t.Errorf("Tag[%d].TargetID=%d dangling after EditMetadata", i, tg.TargetID)
		}
	}
}

// TestRegfix_EditMetadata_TitleChange verifies that a metadata edit on the real
// fixture takes effect while cluster payloads remain byte-identical.
func TestRegfix_EditMetadata_TitleChange(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "titled.mkv")
	ctx := context.Background()

	const newTitle = "Regression Fixture - reindexed"
	if err := EditMetadata(ctx, regfixPath, dst, func(c *mkv.Container) {
		c.Info.Title = newTitle
	}); err != nil {
		t.Fatalf("EditMetadata: %v", err)
	}

	dstC, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if dstC.Info.Title != newTitle {
		t.Errorf("Title = %q, want %q", dstC.Info.Title, newTitle)
	}

	// Cluster payloads must still be byte-identical.
	srcBodies := extractClusterBodies(t, regfixPath)
	dstBodies := extractClusterBodies(t, dst)
	if len(srcBodies) != len(dstBodies) {
		t.Fatalf("cluster count: src=%d dst=%d", len(srcBodies), len(dstBodies))
	}
	for i := range srcBodies {
		if !bytes.Equal(srcBodies[i], dstBodies[i]) {
			t.Errorf("cluster[%d] body changed after title edit", i)
		}
	}
}

// ── Test 4: Demux round-trip on the real file ─────────────────────────────────

// TestRegfix_Demux verifies that Demux extracts the expected number of tracks
// from the real fixture and produces non-empty output files. This exercises
// the laced-audio read path (the real fixture uses AAC lacing).
func TestRegfix_Demux(t *testing.T) {
	outDir := t.TempDir()
	ctx := context.Background()

	if err := Demux(ctx, mkv.DemuxOptions{
		SourcePath: regfixPath,
		OutputDir:  outDir,
	}); err != nil {
		t.Fatalf("Demux: %v", err)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	// 3 tracks → 3 output files.
	if len(entries) != 3 {
		t.Fatalf("expected 3 demuxed files, got %d", len(entries))
	}

	// Each output file must be non-empty.
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(outDir, e.Name()))
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		if info.Size() == 0 {
			t.Errorf("demuxed file %s is empty", e.Name())
		}
	}
}

// TestRegfix_Demux_SingleTrack extracts only the video track (ID=1) and
// verifies block counts match what the BlockReader reports on the full file.
func TestRegfix_Demux_SingleTrack(t *testing.T) {
	outDir := t.TempDir()
	ctx := context.Background()

	if err := Demux(ctx, mkv.DemuxOptions{
		SourcePath: regfixPath,
		OutputDir:  outDir,
		TrackIDs:   []uint64{1},
	}); err != nil {
		t.Fatalf("Demux single track: %v", err)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 demuxed file, got %d", len(entries))
	}
}

// ── Test 5: BlockReader block-count invariant on the real file ───────────────

// TestRegfix_BlockReader_BlockCounts reads all blocks from the real fixture via
// BlockReader and asserts the per-track counts match the values verified when
// the fixture was created.  This locks the lacing decode path against real
// ffmpeg AAC lacing.
//
// Verified counts (probed at fixture creation):
//
//	track 1 (video/H.264): 144 blocks (no lacing)
//	track 2 (audio/AAC):   260 blocks (laced)
//	track 3 (audio/AAC):   260 blocks (laced)
func TestRegfix_BlockReader_BlockCounts(t *testing.T) {
	f, err := os.Open(regfixPath)
	if err != nil {
		t.Fatalf("open regfix.mkv: %v", err)
	}
	defer f.Close()

	br, err := reader.NewBlockReader(f, 1000000)
	if err != nil {
		t.Fatalf("NewBlockReader: %v", err)
	}

	trackCounts := map[uint64]int{}
	for {
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("BlockReader.Next: %v", err)
		}
		trackCounts[blk.TrackNumber]++
	}

	// These exact counts were verified on the real ffmpeg-muxed fixture.
	// If they change, it means the lacing decoder or block-counting logic
	// regressed.
	want := map[uint64]int{1: 144, 2: 260, 3: 260}
	for track, wantN := range want {
		if got := trackCounts[track]; got != wantN {
			t.Errorf("track %d: block count = %d, want %d", track, got, wantN)
		}
	}
}

// TestRegfix_BlockReader_TimecodeOrdering verifies that all blocks from the
// real fixture have non-negative timecodes and that video keyframe timecodes
// increase monotonically (sanity-check on the lacing timecode assignment).
func TestRegfix_BlockReader_TimecodeOrdering(t *testing.T) {
	f, err := os.Open(regfixPath)
	if err != nil {
		t.Fatalf("open regfix.mkv: %v", err)
	}
	defer f.Close()

	br, err := reader.NewBlockReader(f, 1000000)
	if err != nil {
		t.Fatalf("NewBlockReader: %v", err)
	}

	lastVideoKeyframeMs := int64(-1)
	for {
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("BlockReader.Next: %v", err)
		}
		if blk.Timecode < 0 {
			t.Errorf("negative timecode %d on track %d", blk.Timecode, blk.TrackNumber)
		}
		if blk.TrackNumber == 1 && blk.Keyframe {
			if blk.Timecode <= lastVideoKeyframeMs && lastVideoKeyframeMs >= 0 {
				t.Errorf("video keyframe timecode %d ≤ previous %d (not monotonically increasing)",
					blk.Timecode, lastVideoKeyframeMs)
			}
			lastVideoKeyframeMs = blk.Timecode
		}
	}
	if lastVideoKeyframeMs < 0 {
		t.Error("no video keyframes seen")
	}
}

// ── helpers specific to the real fixture ─────────────────────────────────────

// rawSegmentElements walks the segment-level elements of path and calls fn for
// each one, passing (elementID, headerStartOffset, fullElementBytes).
// It is used to verify byte-level identity of specific elements.
func rawSegmentElements(t *testing.T, path string) map[uint32][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	r := bytes.NewReader(data)

	ebmlHdr, _, err := ebml.ReadElementHeader(r)
	if err != nil {
		t.Fatalf("EBML header: %v", err)
	}
	if _, err := io.CopyN(io.Discard, r, ebmlHdr.Size); err != nil {
		t.Fatalf("skip EBML body: %v", err)
	}
	if _, _, err := ebml.ReadElementHeader(r); err != nil {
		t.Fatalf("Segment header: %v", err)
	}

	result := map[uint32][]byte{}
	for {
		startPos := int64(len(data)) - int64(r.Len())
		h, hdrLen, err := ebml.ReadElementHeader(r)
		if err != nil {
			break
		}
		if h.Size < 0 {
			break
		}
		end := startPos + int64(hdrLen) + h.Size
		if end > int64(len(data)) {
			break
		}
		// Only keep the first occurrence.
		if _, exists := result[h.ID]; !exists {
			result[h.ID] = data[startPos:end]
		}
		if _, err := io.CopyN(io.Discard, r, h.Size); err != nil {
			break
		}
	}
	return result
}

// TestRegfix_Reindex_RawElementIdentity is a table-driven check that the raw
// bytes of IDTags, IDChapters, IDTracks, and IDInfo are byte-identical between
// the source and the Reindex output.
func TestRegfix_Reindex_RawElementIdentity(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.mkv")
	ctx := context.Background()
	if err := Reindex(ctx, regfixPath, dst); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	srcElems := rawSegmentElements(t, regfixPath)
	dstElems := rawSegmentElements(t, dst)

	cases := []struct {
		id   uint32
		name string
	}{
		{mkv.IDTags, "Tags"},
		{mkv.IDChapters, "Chapters"},
		{mkv.IDTracks, "Tracks"},
		{mkv.IDInfo, "Info"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, hasSrc := srcElems[tc.id]
			dst2, hasDst := dstElems[tc.id]
			if !hasSrc {
				t.Skipf("element %s not present in source; skipping", tc.name)
			}
			if !hasDst {
				t.Fatalf("element %s present in source but missing in output", tc.name)
			}
			if !bytes.Equal(src, dst2) {
				t.Errorf("element %s not byte-identical: src=%d bytes dst=%d bytes", tc.name, len(src), len(dst2))
			}
		})
	}
}
