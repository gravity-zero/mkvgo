package ops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// writeTaggedMKV is buildMultiClusterMKV plus a tag list.
func writeTaggedMKV(t *testing.T, path string, tracks []mkv.Track, blockSets [][]mkv.Block, tags []mkv.Tag, durationMs int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test"},
		Tags: tags,
	}, tracks, durationMs))
	for _, blocks := range blockSets {
		if len(blocks) == 0 {
			continue
		}
		mustNil(t, mw.WriteClusterWithCues(blocks[0].Timecode, 1_000_000, blocks))
	}
	mustNil(t, mw.Finalize())
}

// hashedFixture writes a two-track file and stamps it with content hashes.
func hashedFixture(t *testing.T, dir, name string) string {
	t.Helper()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	var sets [][]mkv.Block
	for tc := int64(0); tc < 10000; tc += 100 {
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: tc, Keyframe: tc%1000 == 0, Data: []byte{byte(tc), 0xAA}},
			{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte{byte(tc), 0xBB}},
		})
	}
	plain := buildMultiClusterMKV(t, dir, "plain_"+name, tracks, sets, 10000)
	hashed := filepath.Join(dir, name)
	if err := WriteContentHashes(context.Background(), plain, hashed); err != nil {
		t.Fatal(err)
	}
	return hashed
}

// TestSplitJoin_ContentHashesDescribeTheOutput: a part carries a slice of the
// source, a joined file carries several sources - neither is described by the
// source's CONTENT_SHA256. Copying it over made mkvgo's own verification report
// mkvgo's own output as corrupt.
func TestSplitJoin_ContentHashesDescribeTheOutput(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	src := hashedFixture(t, dir, "src.mkv")
	if mm, err := VerifyContentHashes(ctx, src); err != nil || len(mm) != 0 {
		t.Fatalf("the fixture must verify: %d mismatches, %v", len(mm), err)
	}

	parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"),
		Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 5000}, {StartMs: 5000, EndMs: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range parts {
		mm, err := VerifyContentHashes(ctx, p)
		if err != nil {
			t.Fatalf("part %d: %v", i+1, err)
		}
		if len(mm) != 0 {
			t.Errorf("part %d fails its own hashes (%d mismatches) - the source's were copied over", i+1, len(mm))
		}
	}

	joined := filepath.Join(dir, "joined.mkv")
	if err := Join(ctx, parts, joined); err != nil {
		t.Fatal(err)
	}
	mm, err := VerifyContentHashes(ctx, joined)
	if err != nil {
		t.Fatal(err)
	}
	if len(mm) != 0 {
		t.Errorf("the joined file fails its own hashes (%d mismatches)", len(mm))
	}

	// And the round trip is provably lossless: same content as the source.
	diffs, err := CompareBlocks(ctx, src, joined)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Errorf("split+join changed the content: %+v", diffs)
	}
}

// TestPlanContentTags keeps the sorting rule explicit: work metadata stays,
// media-derived families are re-measured, and an emptied tag disappears.
func TestPlanContentTags(t *testing.T) {
	plan := planContentTags([]mkv.Tag{
		{TargetID: 1, SimpleTags: []mkv.SimpleTag{
			{Name: "TITLE", Value: "Ep 1"},
			{Name: ContentHashTag, Value: "deadbeef"},
		}},
		{TargetID: 2, SimpleTags: []mkv.SimpleTag{
			{Name: "BPS", Value: "128000"},
			{Name: "NUMBER_OF_FRAMES", Value: "42"},
		}},
	})
	if !plan.wantHashes || !plan.wantStats {
		t.Errorf("families detected: hashes=%v stats=%v, want both", plan.wantHashes, plan.wantStats)
	}
	if len(plan.kept) != 1 || len(plan.kept[0].SimpleTags) != 1 || plan.kept[0].SimpleTags[0].Name != "TITLE" {
		t.Errorf("kept = %+v, want the TITLE tag alone", plan.kept)
	}
	if plan.digestsFor() == nil || plan.statsFor() == nil {
		t.Error("accumulators must be allocated when the families are present")
	}

	// A source with no content-derived tag must not start hashing anything.
	none := planContentTags([]mkv.Tag{{TargetID: 1, SimpleTags: []mkv.SimpleTag{{Name: "TITLE", Value: "x"}}}})
	if none.recompute() || none.digestsFor() != nil || none.statsFor() != nil {
		t.Error("a source without content tags must not trigger a recompute")
	}
}

// TestSplit_StatisticsAreRemeasured: the statistics family describes the media,
// so a part must state its own - not the whole film's frame count.
func TestSplit_StatisticsAreRemeasured(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	var sets [][]mkv.Block
	for tc := int64(0); tc < 10000; tc += 100 {
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: tc, Keyframe: tc%1000 == 0, Data: []byte("v")},
			{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")},
		})
	}
	src := filepath.Join(dir, "src.mkv")
	writeTaggedMKV(t, src, tracks, sets, []mkv.Tag{{TargetID: 1, SimpleTags: []mkv.SimpleTag{
		{Name: "NUMBER_OF_FRAMES", Value: "100"},
	}}}, 10000)

	parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "p"),
		Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 5000}, {StartMs: 5000, EndMs: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := reader.OpenMeta(ctx, parts[0], reader.WithTags())
	if err != nil {
		t.Fatal(err)
	}
	var frames string
	for _, tag := range c.Tags {
		for _, st := range tag.SimpleTags {
			if st.Name == "NUMBER_OF_FRAMES" && tag.TargetID == 1 {
				frames = st.Value
			}
		}
	}
	if frames == "100" {
		t.Error("the part still declares the source's 100 frames")
	}
	if frames != "50" {
		t.Errorf("NUMBER_OF_FRAMES = %q, want \"50\" (the part's own video frames)", frames)
	}
}

// TestCompareBlocksConcat proves a split lost nothing WITHOUT joining the parts
// back - the point of the N-way form: no temporary copy of a 2 GB film.
func TestCompareBlocksConcat(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	var sets [][]mkv.Block
	for base := int64(0); base < 12000; base += 1000 {
		var cluster []mkv.Block
		for tc := base; tc < base+1000; tc += 100 {
			cluster = append(cluster,
				mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: tc%2000 == 0, Data: []byte{byte(tc / 100), 0x11}},
				mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte{byte(tc / 100), 0x22}})
		}
		sets = append(sets, cluster)
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", tracks, sets, 12000)

	parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "p"),
		Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 4000}, {StartMs: 4000, EndMs: 8000}, {StartMs: 8000, EndMs: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 {
		t.Fatalf("parts = %v", parts)
	}
	diffs, err := CompareBlocksConcat(ctx, src, parts)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Errorf("the three parts should hold exactly the source's content: %+v", diffs)
	}

	// Order matters: the same parts shuffled are NOT the source.
	shuffled := []string{parts[1], parts[0], parts[2]}
	if diffs, err := CompareBlocksConcat(ctx, src, shuffled); err != nil {
		t.Fatal(err)
	} else if len(diffs) == 0 {
		t.Error("parts given out of order should not compare equal")
	}

	// A missing part is caught too.
	if diffs, err := CompareBlocksConcat(ctx, src, parts[:2]); err != nil {
		t.Fatal(err)
	} else if len(diffs) == 0 {
		t.Error("a missing part should show up as a content diff")
	}

	if _, err := CompareBlocksConcat(ctx, src, nil); err == nil {
		t.Error("no parts at all must be an error, not an empty match")
	}
}

// TestSplit_StatisticsWrittenEvenWhenSourceHadNone: the statistics come free
// with a walk the op is doing anyway, so a part states its own bitrate and
// frame count even if nobody ever tagged the source. That matters beyond
// tidiness: WithBitrate has no other way to report a Matroska track's bitrate
// on the metadata-only path.
func TestSplit_StatisticsWrittenEvenWhenSourceHadNone(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	var sets [][]mkv.Block
	for base := int64(0); base < 10000; base += 1000 {
		var cluster []mkv.Block
		for tc := base; tc < base+1000; tc += 100 {
			cluster = append(cluster,
				mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: tc%1000 == 0, Data: make([]byte, 500)},
				mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: make([]byte, 50)})
		}
		sets = append(sets, cluster)
	}
	// No tags whatsoever on the source.
	src := buildMultiClusterMKV(t, dir, "src.mkv", tracks, sets, 10000)
	if c, err := reader.OpenMeta(ctx, src, reader.WithTags()); err != nil {
		t.Fatal(err)
	} else if len(c.Tags) != 0 {
		t.Fatalf("the fixture must start untagged, has %d", len(c.Tags))
	}

	parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "p"),
		Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 5000}, {StartMs: 5000, EndMs: 0}}})
	if err != nil {
		t.Fatal(err)
	}

	c, err := reader.OpenMeta(ctx, parts[0], reader.WithTags())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tag := range c.Tags {
		if tag.TargetID != 1 {
			continue
		}
		for _, st := range tag.SimpleTags {
			got[st.Name] = st.Value
		}
	}
	// Half the film: 50 video frames of 500 bytes.
	if got["NUMBER_OF_FRAMES"] != "50" {
		t.Errorf("NUMBER_OF_FRAMES = %q, want \"50\"", got["NUMBER_OF_FRAMES"])
	}
	if got["NUMBER_OF_BYTES"] != "25000" {
		t.Errorf("NUMBER_OF_BYTES = %q, want \"25000\"", got["NUMBER_OF_BYTES"])
	}
	if got["BPS"] == "" || got["DURATION"] == "" {
		t.Errorf("BPS/DURATION missing: %v", got)
	}

	// The head-only bitrate path finds them - the reason this is not cosmetic.
	withBitrate, err := reader.OpenMeta(ctx, parts[0], reader.WithBitrate())
	if err != nil {
		t.Fatal(err)
	}
	if withBitrate.Tracks[0].Bitrate == nil {
		t.Error("WithBitrate found no bitrate on the part - the statistics tags are its only source")
	}

	// And the content is untouched by any of this.
	if diffs, err := CompareBlocksConcat(ctx, src, parts); err != nil {
		t.Fatal(err)
	} else if len(diffs) != 0 {
		t.Errorf("content changed: %+v", diffs)
	}
}
