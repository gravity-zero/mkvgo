package ops

// mutation_kill_test.go — boundary / negation tests that kill surviving mutants
// reported by the gremlins run. Each test exercises the EXACT operator boundary
// so the mutated operator produces a different observable result.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// collectTimecodes returns block timecodes (in order) for a given trackID.
func collectTimecodes(t *testing.T, path string, timecodeScale int64, trackID uint64) []int64 {
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
	var out []int64
	for {
		blk, err := br.Next()
		if err != nil {
			break
		}
		if blk.TrackNumber == trackID {
			out = append(out, blk.Timecode)
		}
	}
	return out
}

// buildCustomTimecodeScaleMKV writes a single-cluster MKV with the requested
// TimecodeScale and returns the path.
func buildCustomTimecodeScaleMKV(t *testing.T, dir, name string, scale int64) string {
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
		Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "test", WritingApp: "test"},
	}
	tracks := []mkv.Track{videoTrack(1)}
	if err := mw.WriteMetadata(c, tracks, 1000); err != nil {
		t.Fatal(err)
	}
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
		{TrackNumber: 1, Timecode: 500, Data: []byte("v1")},
	}
	if err := mw.WriteClusterWithCues(0, scale, blocks); err != nil {
		t.Fatal(err)
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// ── stream.go: timeStart / timeEnd boundary tests ────────────────────────────

// TestSplit_BlockAtExactTimeStartIncluded kills stream.go line 76
// (blk.Timecode < opts.timeStart vs <= opts.timeStart).
// The block AT exactly timeStart must be included in the split output.
func TestSplit_BlockAtExactTimeStartIncluded(t *testing.T) {
	dir := t.TempDir()
	src := buildMinimalMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[]mkv.Block{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
			{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v1")},
			{TrackNumber: 1, Timecode: 2000, Keyframe: true, Data: []byte("v2")},
		},
		3000,
	)
	outDir := filepath.Join(dir, "parts")
	ctx := context.Background()
	files, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src,
		OutputDir:  outDir,
		Ranges:     []mkv.TimeRange{{StartMs: 1000, EndMs: 3000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 output file, got %d", len(files))
	}
	// Block at 1000ms (= StartMs) must be present.
	// Mutation (<= startMs): block at 1000ms is skipped → only 2000ms block → count=1.
	counts := countBlocksFromFile(t, files[0], 1_000_000)
	if counts[1] != 2 {
		t.Errorf("split starting at 1000ms: expected 2 blocks (1000 and 2000ms), got %d", counts[1])
	}
}

// TestSplit_OpenEndedRangeIncludesAllBlocks kills stream.go line 83 first condition
// (opts.timeEnd > 0 vs >= 0).
// EndMs == 0 means "no end": all blocks from StartMs onwards must be in the output.
func TestSplit_OpenEndedRangeIncludesAllBlocks(t *testing.T) {
	dir := t.TempDir()
	src := buildMinimalMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[]mkv.Block{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
			{TrackNumber: 1, Timecode: 500, Keyframe: true, Data: []byte("v1")},
			{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v2")},
		},
		1500,
	)
	outDir := filepath.Join(dir, "parts")
	ctx := context.Background()
	files, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src,
		OutputDir:  outDir,
		Ranges:     []mkv.TimeRange{{StartMs: 0, EndMs: 0}}, // EndMs=0 → open-ended
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 output file, got %d", len(files))
	}
	// Mutation (>= 0): timeEnd=0, 0>=0=true → check blk.TC>=0 immediately → break → 0 blocks.
	counts := countBlocksFromFile(t, files[0], 1_000_000)
	if counts[1] != 3 {
		t.Errorf("open-ended split should include all 3 blocks, got %d", counts[1])
	}
}

// TestSplit_BlockAtExactTimeEndExcluded kills stream.go line 83 second condition
// (blk.Timecode >= opts.timeEnd vs > opts.timeEnd).
// The block AT exactly EndMs must NOT appear in the output.
func TestSplit_BlockAtExactTimeEndExcluded(t *testing.T) {
	dir := t.TempDir()
	src := buildMinimalMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[]mkv.Block{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
			{TrackNumber: 1, Timecode: 500, Keyframe: true, Data: []byte("v1")},
			{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v2")},
		},
		1500,
	)
	outDir := filepath.Join(dir, "parts")
	ctx := context.Background()
	files, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src,
		OutputDir:  outDir,
		Ranges:     []mkv.TimeRange{{StartMs: 0, EndMs: 1000}}, // [0, 1000): 1000ms is exclusive
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 output file, got %d", len(files))
	}
	// Block at 1000ms (= EndMs) must be absent: original >= excludes it.
	// Mutation (>): 1000 > 1000 = false → included → count becomes 3.
	counts := countBlocksFromFile(t, files[0], 1_000_000)
	if counts[1] != 2 {
		t.Errorf("split [0,1000) should include 2 blocks (0 and 500ms), got %d; boundary block must be excluded", counts[1])
	}
	// Also verify 1000ms timecode does not appear.
	tcs := collectTimecodes(t, files[0], 1_000_000, 1)
	for _, tc := range tcs {
		if tc >= 1000 {
			t.Errorf("block at %dms should not appear in [0,1000) split", tc)
		}
	}
}

// TestStreamToWriter_ClusterFlushAtExactBoundary kills stream.go line 102
// (blk.Timecode-clusterTS >= defaultClusterDurationMs vs >).
// Blocks exactly 1000ms apart must flush into separate clusters, producing 2+ cues.
func TestStreamToWriter_ClusterFlushAtExactBoundary(t *testing.T) {
	dir := t.TempDir()
	// Two keyframes exactly 1000ms apart (= defaultClusterDurationMs).
	// Original (>=): flush triggered → 2 clusters → 2 cues.
	// Mutation (>): 1000-0=1000 > 1000 = false → same cluster → 1 cue.
	src := buildMinimalMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[]mkv.Block{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
			{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v1")},
		},
		2000,
	)
	srtPath := filepath.Join(dir, "sub.srt")
	os.WriteFile(srtPath, []byte("1\n00:00:00,000 --> 00:00:01,000\nHi\n\n"), 0644)
	dst := filepath.Join(dir, "out.mkv")
	ctx := context.Background()
	if err := MergeSubtitle(ctx, src, srtPath, dst, "eng", "Sub"); err != nil {
		t.Fatal(err)
	}
	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	// WriteClusterWithCues records one cue per cluster with a keyframe.
	// Two clusters (one per keyframe) → at least 2 cues.
	if len(c.Cues) < 2 {
		t.Errorf("two keyframes exactly 1000ms apart should produce >=2 cues (got %d); cluster-flush boundary may be off-by-one", len(c.Cues))
	}
}

// TestStreamMergeToWriter_ClusterFlushAtExactBoundary kills stream.go line 231
// (streamMergeToWriter: same >= condition as streamToWriter).
func TestStreamMergeToWriter_ClusterFlushAtExactBoundary(t *testing.T) {
	dir := t.TempDir()
	srcA := filepath.Join(dir, "a.mkv")
	srcB := filepath.Join(dir, "b.mkv")
	writeTrackMKV(t, srcA, "vp9", []int64{0, 1000}) // keyframes at 0 and 1000ms
	writeTrackMKV(t, srcB, "opus", []int64{0, 1000})
	dst := filepath.Join(dir, "muxed.mkv")

	mustNil(t, Mux(context.Background(), mkv.MuxOptions{
		OutputPath: dst,
		Tracks: []mkv.TrackInput{
			{SourcePath: srcA, TrackID: 1},
			{SourcePath: srcB, TrackID: 1},
		},
	}))

	c, err := reader.Open(context.Background(), dst)
	if err != nil {
		t.Fatal(err)
	}
	// With exact-1000ms spacing, cluster flush should fire → >=2 cues.
	// Mutation (>): no flush at exactly 1000ms → 1 cue.
	if len(c.Cues) < 2 {
		t.Errorf("mux with keyframes at 0 and 1000ms should produce >=2 cues, got %d", len(c.Cues))
	}
}

// ── reindex.go: cue throttle boundary ────────────────────────────────────────

// TestReindex_AudioCueThrottleExactBoundary kills the >= condition in
// appendCueFromCluster (reindex.go) and WriteClusterWithCues (mkvwriter.go)
// (firstBlockTC - lastCueTime >= reindexCueMinGapMs vs >).
// Clusters exactly 500ms apart must all produce cues.
func TestReindex_AudioCueThrottleExactBoundary(t *testing.T) {
	dir := t.TempDir()
	// Three audio-only clusters at 0, 500, 1000ms — each exactly 500ms apart.
	// Original (>=): 500-0=500 >= 500 → cue; 1000-500=500 >= 500 → cue → 3 cues.
	// Mutation (>): 500-0=500 > 500 = false → skip 500ms cue → only 2 cues.
	src := buildMultiClusterMKV(t, dir, "audio.mkv",
		[]mkv.Track{audioTrack(1)},
		[][]mkv.Block{
			{{TrackNumber: 1, Timecode: 0, Keyframe: false, Data: []byte("a0")}},
			{{TrackNumber: 1, Timecode: 500, Keyframe: false, Data: []byte("a500")}},
			{{TrackNumber: 1, Timecode: 1000, Keyframe: false, Data: []byte("a1000")}},
		},
		1500,
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
	if len(c.Cues) < 3 {
		t.Errorf("3 audio clusters exactly 500ms apart should produce 3 cues, got %d (throttle boundary off-by-one?)", len(c.Cues))
	}
	assertCuesPointToClusters(t, dst, c.Cues)
}

// ── validate.go: timecode boundary ───────────────────────────────────────────

// TestValidate_TimecodeBackwardExactToleranceNotFlagged kills validate.go line 107
// (blk.Timecode < lastTC-1000 vs <= lastTC-1000).
// A block exactly 1000ms before the previous one is within tolerance: no issue.
func TestValidate_TimecodeBackwardExactToleranceNotFlagged(t *testing.T) {
	dir := t.TempDir()
	// Write blocks out of order: 1000ms then 0ms (exactly 1000ms backward).
	// In validate: lastTC=1000, blk.TC=0. 0 < 1000-1000 = 0 < 0 = false → no issue.
	// Mutation (<=): 0 <= 0 = true → issue raised → test fails.
	src := buildMinimalMKV(t, dir, "ooo.mkv",
		[]mkv.Track{audioTrack(1)},
		[]mkv.Block{
			{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("a1")},
			{TrackNumber: 1, Timecode: 0, Keyframe: false, Data: []byte("a0")},
		},
		1500,
	)
	ctx := context.Background()
	issues, err := Validate(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	for _, iss := range issues {
		if strings.Contains(iss.Message, "timecode went backwards") {
			t.Errorf("unexpected 'timecode went backwards' issue for exact-1000ms gap: %s", iss.Message)
		}
	}
}

// TestValidate_TimecodeBackwardBeyondToleranceFlagged verifies that a jump MORE
// than 1000ms backward IS flagged (tests the positive side of the boundary).
func TestValidate_TimecodeBackwardBeyondToleranceFlagged(t *testing.T) {
	dir := t.TempDir()
	// 2000ms then 500ms: 2000-500=1500ms backward > 1000ms tolerance → flagged.
	src := buildMinimalMKV(t, dir, "ooo2.mkv",
		[]mkv.Track{audioTrack(1)},
		[]mkv.Block{
			{TrackNumber: 1, Timecode: 2000, Keyframe: true, Data: []byte("a2")},
			{TrackNumber: 1, Timecode: 500, Keyframe: false, Data: []byte("a0")},
		},
		2500,
	)
	ctx := context.Background()
	issues, err := Validate(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, iss := range issues {
		if strings.Contains(iss.Message, "timecode went backwards") {
			found = true
		}
	}
	if !found {
		t.Error("expected 'timecode went backwards' issue for 1500ms backward jump")
	}
}

// TestValidate_SmallBackwardJumpNotFlagged kills validate.go line 107 arithmetic
// (lastTC - 1000 vs lastTC + 1000).
// A small backward jump (< 1000ms) is within tolerance and must not be flagged.
func TestValidate_SmallBackwardJumpNotFlagged(t *testing.T) {
	dir := t.TempDir()
	// 1000ms then 500ms: 500ms backward. Within 1000ms tolerance → no issue.
	// If arithmetic mutated to +1000: 500 < 1000+1000=2000 = true → wrongly flagged.
	src := buildMinimalMKV(t, dir, "small_bwd.mkv",
		[]mkv.Track{audioTrack(1)},
		[]mkv.Block{
			{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("a1")},
			{TrackNumber: 1, Timecode: 500, Keyframe: false, Data: []byte("a0")},
		},
		1500,
	)
	ctx := context.Background()
	issues, err := Validate(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	for _, iss := range issues {
		if strings.Contains(iss.Message, "timecode went backwards") {
			t.Errorf("unexpected backward-timecode issue for 500ms backward jump (within tolerance): %s", iss.Message)
		}
	}
}

// TestValidate_NoKeyframesIssueOnlyWhenBlocksExist kills validate.go line 116
// (!hasKeyframe && blockTotal > 0 vs >= 0).
// A file with zero blocks must NOT produce "no keyframes found" (blockTotal == 0).
func TestValidate_NoKeyframesIssueOnlyWhenBlocksExist(t *testing.T) {
	dir := t.TempDir()
	// File with 0 blocks but valid metadata. hasKeyframe=false, blockTotal=0.
	// Original: false && 0 > 0 = false → no issue.
	// Mutation (>= 0): false && 0 >= 0 = true → wrongly raises issue.
	src := buildMinimalMKV(t, dir, "noblocks.mkv",
		[]mkv.Track{videoTrack(1)}, // has tracks, but no blocks written
		nil,
		0,
	)
	ctx := context.Background()
	issues, err := Validate(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	for _, iss := range issues {
		if strings.Contains(iss.Message, "no keyframes found") {
			t.Errorf("file with 0 blocks must not produce 'no keyframes found': %s", iss.Message)
		}
	}
}

// TestValidate_NoKeyframesIssueRaisedWithNonKeyframeBlocks verifies the positive
// case: blocks exist but none are keyframes → "no keyframes found" IS raised.
func TestValidate_NoKeyframesIssueRaisedWithNonKeyframeBlocks(t *testing.T) {
	dir := t.TempDir()
	src := buildMinimalMKV(t, dir, "nokf.mkv",
		[]mkv.Track{videoTrack(1)},
		[]mkv.Block{
			{TrackNumber: 1, Timecode: 0, Keyframe: false, Data: []byte("v0")},
			{TrackNumber: 1, Timecode: 100, Keyframe: false, Data: []byte("v1")},
		},
		200,
	)
	ctx := context.Background()
	issues, err := Validate(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, iss := range issues {
		if strings.Contains(iss.Message, "no keyframes found") {
			found = true
		}
	}
	if !found {
		t.Error("expected 'no keyframes found' when blocks exist but none are keyframes")
	}
}

// ── compare.go: chapter time boundary ────────────────────────────────────────

// TestCompareChapters_SameStartDifferentEnd kills compare.go line 102
// (a[i].EndMs != b[i].EndMs component of the || condition).
func TestCompareChapters_SameStartDifferentEnd(t *testing.T) {
	a := []mkv.Chapter{{ID: 1, StartMs: 0, EndMs: 1000}}
	b := []mkv.Chapter{{ID: 1, StartMs: 0, EndMs: 2000}} // same start, different end
	diffs := compareChapters(a, b)
	found := false
	for _, d := range diffs {
		if strings.Contains(d.Section, "chapter[1].time") {
			found = true
		}
	}
	if !found {
		t.Error("expected chapter[1].time diff for same StartMs but different EndMs")
	}
}

// TestCompareChapters_DifferentStartSameEnd kills compare.go line 102
// (a[i].StartMs != b[i].StartMs component of the || condition).
func TestCompareChapters_DifferentStartSameEnd(t *testing.T) {
	a := []mkv.Chapter{{ID: 1, StartMs: 0, EndMs: 1000}}
	b := []mkv.Chapter{{ID: 1, StartMs: 500, EndMs: 1000}} // different start, same end
	diffs := compareChapters(a, b)
	found := false
	for _, d := range diffs {
		if strings.Contains(d.Section, "chapter[1].time") {
			found = true
		}
	}
	if !found {
		t.Error("expected chapter[1].time diff for different StartMs but same EndMs")
	}
}

// TestCompareChapters_SameTimesNoDiff kills || → && mutation in compare.go line 102.
// Identical chapter times must produce no time diff.
func TestCompareChapters_SameTimesNoDiff(t *testing.T) {
	a := []mkv.Chapter{{ID: 1, StartMs: 100, EndMs: 500}}
	b := []mkv.Chapter{{ID: 1, StartMs: 100, EndMs: 500}}
	diffs := compareChapters(a, b)
	for _, d := range diffs {
		if strings.Contains(d.Section, "time") {
			t.Errorf("unexpected time diff for identical chapter times: %+v", d)
		}
	}
}

// TestCompareTracks_AddedTrackAtExactBoundary kills compare.go lines 56-57
// (i >= len(a) vs i > len(a)).
// When b has one more track than a, the extra track must produce a DiffAdded.
func TestCompareTracks_AddedTrackAtExactBoundary(t *testing.T) {
	a := []mkv.Track{videoTrack(1)}
	b := []mkv.Track{videoTrack(1), audioTrack(2)}
	diffs := compareTracks(a, b)
	found := false
	for _, d := range diffs {
		if d.Type == mkv.DiffAdded && strings.Contains(d.Section, "track[2]") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected DiffAdded for track[2] when b has one more track; got: %v", diffs)
	}
}

// TestCompareTracks_RemovedTrackAtExactBoundary kills compare.go lines 60-61
// (i >= len(b) vs i > len(b)).
// When a has one more track than b, the extra track must produce a DiffRemoved.
func TestCompareTracks_RemovedTrackAtExactBoundary(t *testing.T) {
	a := []mkv.Track{videoTrack(1), audioTrack(2)}
	b := []mkv.Track{videoTrack(1)}
	diffs := compareTracks(a, b)
	found := false
	for _, d := range diffs {
		if d.Type == mkv.DiffRemoved && strings.Contains(d.Section, "track[2]") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected DiffRemoved for track[2] when a has one more track; got: %v", diffs)
	}
}

// ── split.go: duration and chaptersToRanges boundaries ───────────────────────

// TestSplit_PartialRangeBlocksCorrect verifies block filtering for a bounded range.
// The line-57 durationMs mutation is equivalent: WriteSegmentInfo prefers c.Info.Duration
// over the durationMs parameter, so the output duration equals the source regardless.
// This test instead guards block-level correctness for the bounded-range path.
func TestSplit_PartialRangeBlocksCorrect(t *testing.T) {
	dir := t.TempDir()
	src := buildMinimalMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[]mkv.Block{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
			{TrackNumber: 1, Timecode: 500, Keyframe: true, Data: []byte("v1")},
			{TrackNumber: 1, Timecode: 2000, Keyframe: true, Data: []byte("v2")},
		},
		3000,
	)
	outDir := filepath.Join(dir, "parts")
	ctx := context.Background()
	files, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src,
		OutputDir:  outDir,
		Ranges:     []mkv.TimeRange{{StartMs: 0, EndMs: 1000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 output file, got %d", len(files))
	}
	counts := countBlocksFromFile(t, files[0], 1_000_000)
	if counts[1] != 2 {
		t.Errorf("split [0,1000): expected 2 blocks (0ms and 500ms), got %d", counts[1])
	}
}

// TestChaptersToRanges_PreservesExplicitEndMs kills split.go line 74
// (ranges[i].EndMs == 0 vs != 0).
// Chapters with explicit EndMs must not have their EndMs overridden.
func TestChaptersToRanges_PreservesExplicitEndMs(t *testing.T) {
	// Chapter[0].EndMs=1500 but chapters[1].StartMs=1000 (deliberately different).
	// Original (== 0): 1500 == 0 is false → EndMs preserved as 1500.
	// Mutation (!= 0): 1500 != 0 is true → EndMs overwritten with 1000. Wrong!
	chapters := []mkv.Chapter{
		{ID: 1, StartMs: 0, EndMs: 1500},
		{ID: 2, StartMs: 1000, EndMs: 2000},
	}
	ranges := chaptersToRanges(chapters)
	if len(ranges) != 2 {
		t.Fatalf("expected 2 ranges, got %d", len(ranges))
	}
	if ranges[0].EndMs != 1500 {
		t.Errorf("chapter[0].EndMs=1500 must be preserved; got %d (mutation may have overridden with next.StartMs=%d)", ranges[0].EndMs, chapters[1].StartMs)
	}
}

// TestChaptersToRanges_LastChapterNoPanic kills split.go line 74
// (i+1 < len(chapters) vs i+1 <= len(chapters)).
// The last chapter (EndMs=0, no next chapter) must not cause an out-of-bounds access.
func TestChaptersToRanges_LastChapterNoPanic(t *testing.T) {
	chapters := []mkv.Chapter{{ID: 1, StartMs: 0}} // single chapter, EndMs=0
	ranges := chaptersToRanges(chapters)
	// Mutation (<=): i+1=1 <= 1=true → accesses chapters[1] → panic.
	if len(ranges) != 1 {
		t.Fatalf("expected 1 range, got %d", len(ranges))
	}
	if ranges[0].EndMs != 0 {
		t.Errorf("last chapter with no next chapter should have EndMs=0, got %d", ranges[0].EndMs)
	}
}

// ── mux.go: timecodeScale and duration boundaries ────────────────────────────

// TestMux_TimecodeScalePreservedFromSource kills mux.go line 128
// (timecodeScale == 0 vs != 0).
// When the source has a non-zero TimecodeScale, the output must use that scale.
func TestMux_TimecodeScalePreservedFromSource(t *testing.T) {
	dir := t.TempDir()
	const distinctScale = int64(500_000) // 0.5ms per tick — not the 1000000 default
	src := buildCustomTimecodeScaleMKV(t, dir, "scaled.mkv", distinctScale)
	dst := filepath.Join(dir, "out.mkv")
	ctx := context.Background()
	if err := Mux(ctx, mkv.MuxOptions{
		OutputPath: dst,
		Tracks:     []mkv.TrackInput{{SourcePath: src, TrackID: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	// Mutation (!= 0): timecodeScale=500000, 500000 != 0 = true → override to 1000000.
	if c.Info.TimecodeScale != distinctScale {
		t.Errorf("output TimecodeScale=%d; want %d from source (mutation may have overridden with default)", c.Info.TimecodeScale, distinctScale)
	}
}

// TestMux_DurationIsMaxOfSources kills mux.go line 132
// (c.DurationMs > durationMs vs < durationMs / == 0).
// Output duration must be the maximum across all input sources.
func TestMux_DurationIsMaxOfSources(t *testing.T) {
	dir := t.TempDir()
	// Source A: 1000ms. Source B: 3000ms. Expected output: 3000ms.
	srcA := buildMinimalMKV(t, dir, "a.mkv",
		[]mkv.Track{videoTrack(1)},
		[]mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")}},
		1000,
	)
	srcB := buildMinimalMKV(t, dir, "b.mkv",
		[]mkv.Track{audioTrack(1)},
		[]mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("a")}},
		3000,
	)
	dst := filepath.Join(dir, "out.mkv")
	ctx := context.Background()
	if err := Mux(ctx, mkv.MuxOptions{
		OutputPath: dst,
		Tracks: []mkv.TrackInput{
			{SourcePath: srcA, TrackID: 1},
			{SourcePath: srcB, TrackID: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	// Mutation (< or ==): would pick min or last source duration (1000 or 3000 in wrong order).
	if c.DurationMs != 3000 {
		t.Errorf("mux duration should be max(1000,3000)=3000, got %d", c.DurationMs)
	}
}

// ── subtitle.go: track filter with multiple tracks ───────────────────────────

// TestExtractSubtitle_FiltersCorrectTrackInMultiTrackFile kills subtitle.go line 63
// (blk.TrackNumber != trackID vs == trackID).
// Only subtitle blocks must appear in the output; video blocks must be excluded.
func TestExtractSubtitle_FiltersCorrectTrackInMultiTrackFile(t *testing.T) {
	dir := t.TempDir()
	// Video track 1 + subtitle track 2 in the same file.
	src := buildMinimalMKV(t, dir, "multi.mkv",
		[]mkv.Track{videoTrack(1), subtitleTrack(2, "srt")},
		[]mkv.Block{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("video_frame")},
			{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("SubText")},
			{TrackNumber: 1, Timecode: 100, Data: []byte("video_frame2")},
		},
		1000,
	)
	outPath := filepath.Join(dir, "out.srt")
	ctx := context.Background()
	if err := ExtractSubtitle(ctx, src, 2, outPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	// Subtitle block must be present.
	if !strings.Contains(content, "SubText") {
		t.Error("subtitle text 'SubText' must appear in extracted SRT")
	}
	// Video block data must not appear (wrong track filtered correctly).
	// With mutation (== trackID): subtitle blocks skipped, video included.
	if strings.Contains(content, "video_frame") {
		t.Error("video frame data must not appear in extracted subtitle SRT")
	}
}

// TestExtractSubtitleWebVTT_FiltersCorrectTrack kills subtitle.go (WebVTT path)
// track filter line.
func TestExtractSubtitleWebVTT_FiltersCorrectTrack(t *testing.T) {
	dir := t.TempDir()
	src := buildMinimalMKV(t, dir, "multi.mkv",
		[]mkv.Track{videoTrack(1), subtitleTrack(2, "srt")},
		[]mkv.Block{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("VID_DATA")},
			{TrackNumber: 2, Timecode: 500, Duration: 1000, Data: []byte("Caption")},
		},
		2000,
	)
	var sb strings.Builder
	ctx := context.Background()
	if err := ExtractSubtitleWebVTT(ctx, src, 2, &sb); err != nil {
		t.Fatal(err)
	}
	got := sb.String()
	if !strings.Contains(got, "Caption") {
		t.Error("WebVTT output must contain 'Caption'")
	}
	if strings.Contains(got, "VID_DATA") {
		t.Error("WebVTT output must not contain video track data")
	}
}

// ── inplace.go: size boundary ─────────────────────────────────────────────────

// TestEditInPlace_ExactFitSucceeds kills inplace.go line 64
// (newSize > available vs >= available).
// When new metadata is exactly the same as old, the fit must succeed.
func TestEditInPlace_ExactFitSucceeds(t *testing.T) {
	dir := t.TempDir()
	// Build a file with known metadata. Then rewrite it with the same metadata
	// (no changes). available >= newSize must always be true here since we
	// rewrite identical content. The mutation (>=) would error when newSize==available.
	src := buildMinimalMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		testBlocks(1),
		500,
	)
	ctx := context.Background()
	// Read current title to rewrite the same value.
	c, err := reader.Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	origTitle := c.Info.Title
	// Write identical metadata: new size == old serialized size (or smaller due to void).
	if err := EditInPlace(ctx, src, func(c *mkv.Container) {
		c.Info.Title = origTitle // no change
	}); err != nil {
		t.Fatalf("EditInPlace with unchanged metadata failed: %v", err)
	}
	// File must still be parseable.
	c2, err := reader.Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Info.Title != origTitle {
		t.Errorf("title changed after no-op EditInPlace: %q", c2.Info.Title)
	}
}

// ── reindex.go: progress and buffer boundaries ───────────────────────────────

// TestReindex_ProgressNotCalledWhenTotalBytesZero kills reindex.go line 129
// (progress != nil && totalBytes > 0 vs >= 0).
// When totalBytes == 0, the progress callback must NOT be invoked.
func TestReindex_ProgressNotCalledWhenTotalBytesZero(t *testing.T) {
	dir := t.TempDir()
	src := buildMultiClusterMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[][]mkv.Block{
			{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")}},
		},
		1000,
	)
	dst := filepath.Join(dir, "dst.mkv")
	dstFile, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer dstFile.Close()

	mw := writer.NewMKVWriter(dstFile)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	info := mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000}}
	if err := mw.WriteMetadata(&info, []mkv.Track{videoTrack(1)}, 1000); err != nil {
		t.Fatal(err)
	}

	var called bool
	progressFn := mkv.ProgressFunc(func(done, total int64) { called = true })
	// Pass totalBytes=0: progress must not fire (totalBytes > 0 = false).
	// Mutation (>= 0): 0 >= 0 = true → progress WOULD fire. Test catches it.
	if err := reindexFastCopy(mw, src, 1_000_000, nil, progressFn, 0, nil); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("progress callback must not be called when totalBytes == 0")
	}
}

// TestReindex_ProgressCalledWhenTotalBytesPositive verifies that progress IS
// called when totalBytes > 0 (both sides of the > 0 boundary).
func TestReindex_ProgressCalledWhenTotalBytesPositive(t *testing.T) {
	dir := t.TempDir()
	src := buildMultiClusterMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[][]mkv.Block{
			{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")}},
		},
		1000,
	)
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	totalBytes := info.Size()

	dst := filepath.Join(dir, "dst.mkv")
	dstFile, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer dstFile.Close()

	mw := writer.NewMKVWriter(dstFile)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	cnt := mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000}}
	if err := mw.WriteMetadata(&cnt, []mkv.Track{videoTrack(1)}, 1000); err != nil {
		t.Fatal(err)
	}

	var calls int
	progressFn := mkv.ProgressFunc(func(done, total int64) { calls++ })
	if err := reindexFastCopy(mw, src, 1_000_000, nil, progressFn, totalBytes, nil); err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Error("progress callback must be called when totalBytes > 0")
	}
}

// TestSplit_OpenEndedRangeWithStartMsIncludesRemaining verifies that an open-ended
// range with a non-zero StartMs includes all blocks from that offset onwards.
func TestSplit_OpenEndedRangeWithStartMsIncludesRemaining(t *testing.T) {
	dir := t.TempDir()
	src := buildMinimalMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[]mkv.Block{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
			{TrackNumber: 1, Timecode: 500, Keyframe: true, Data: []byte("v1")},
			{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("v2")},
			{TrackNumber: 1, Timecode: 1500, Keyframe: true, Data: []byte("v3")},
		},
		2000,
	)
	outDir := filepath.Join(dir, "parts")
	ctx := context.Background()
	files, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src,
		OutputDir:  outDir,
		Ranges:     []mkv.TimeRange{{StartMs: 500, EndMs: 0}}, // open-ended from 500ms
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 output file, got %d", len(files))
	}
	// Blocks at 500, 1000, 1500ms must be in the output; 0ms block excluded.
	counts := countBlocksFromFile(t, files[0], 1_000_000)
	if counts[1] != 3 {
		t.Errorf("open-ended split from 500ms: expected 3 blocks, got %d", counts[1])
	}
}
