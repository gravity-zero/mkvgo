package ops

// ingest_test.go exercises Ingest end to end: the decision mapping
// (direct-play/remux-hls/transcode), the seek-index/reindex branch (with and
// without an existing Cues index, in-place-capable and not), IncludeAnalysis,
// unknown target, FS port and ctx cancellation.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
	"github.com/gravity-zero/mkvgo/mp4"
)

// buildNoCuesFrontSlotMKV writes a video+audio MKV whose writer reserves the
// usual front SeekHead slot but ends up with NO Cues element (mw.Cues is
// cleared before Finalize): a source with a discoverable head SeekHead but no
// seek index yet, the case ReindexInPlace can patch in place.
func buildNoCuesFrontSlotMKV(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "nocues.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	if err := mw.WriteMetadata(c, tracks, 2000); err != nil {
		t.Fatal(err)
	}
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: bytes.Repeat([]byte("X"), 64)},
		{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("audio")},
	}
	if err := mw.WriteClusterWithCues(0, 1_000_000, blocks); err != nil {
		t.Fatal(err)
	}
	mw.Cues = nil
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIngest_MP4Source_DirectPlay(t *testing.T) {
	dir := t.TempDir()
	dstMP4 := filepath.Join(dir, "out.mp4")
	if err := mp4.RemuxToMP4(context.Background(), sampleMKV, dstMP4); err != nil {
		t.Fatal(err)
	}

	plan, err := Ingest(context.Background(), dstMP4, IngestOptions{Target: "mse-generic"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Strategy != StrategyDirectPlay {
		t.Fatalf("strategy = %q, want direct-play (reasons=%v)", plan.Strategy, plan.Reasons)
	}
	if plan.SourceContainer != "mp4" {
		t.Fatalf("source container = %q, want mp4", plan.SourceContainer)
	}
	if plan.RemuxContainer != "" {
		t.Fatalf("remux container = %q, want empty for direct-play", plan.RemuxContainer)
	}
	if plan.NeedsReindex || plan.Reindexed {
		t.Fatalf("direct-play must not consider a reindex: needs=%v reindexed=%v", plan.NeedsReindex, plan.Reindexed)
	}
	if len(plan.Reasons) == 0 {
		t.Fatal("expected a non-empty reasons trail")
	}
}

func TestIngest_MKVSource_RemuxHLS_WithCues(t *testing.T) {
	// sampleMKV is a real fixture with a Cues index already built in.
	plan, err := Ingest(context.Background(), sampleMKV, IngestOptions{Target: "mse-generic"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Strategy != StrategyRemuxHLS {
		t.Fatalf("strategy = %q, want remux-hls (reasons=%v)", plan.Strategy, plan.Reasons)
	}
	if plan.RemuxContainer != "mp4" && plan.RemuxContainer != "hls" {
		t.Fatalf("remux container = %q, want mp4 or hls", plan.RemuxContainer)
	}
	if !plan.HasSeekIndex {
		t.Fatal("expected HasSeekIndex true (sampleMKV already carries Cues)")
	}
	if plan.NeedsReindex {
		t.Fatal("expected NeedsReindex false when a seek index already exists")
	}
}

func TestIngest_MKVSource_RemuxHLS_NoCues_NeedsReindex(t *testing.T) {
	dir := t.TempDir()
	path := buildNoCuesFrontSlotMKV(t, dir)

	// safari has no H.264 level ceiling, so this synthetic track (whose
	// placeholder CodecPrivate carries no real SPS level to parse) still
	// verdicts remux (mkv source vs. safari's mp4/hls containers) instead of
	// a level-driven transcode.
	plan, err := Ingest(context.Background(), path, IngestOptions{Target: "safari"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Strategy != StrategyRemuxHLS {
		t.Fatalf("strategy = %q, want remux-hls (reasons=%v)", plan.Strategy, plan.Reasons)
	}
	if plan.HasSeekIndex {
		t.Fatal("expected HasSeekIndex false (no Cues written)")
	}
	if !plan.NeedsReindex {
		t.Fatal("expected NeedsReindex true")
	}
	if plan.Reindexed {
		t.Fatal("opts.Reindex was not set: Ingest must not reindex on its own")
	}
	found := false
	for _, r := range plan.Reasons {
		if strings.Contains(r, "reindex") {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons = %v, want a mention of the reindex requirement", plan.Reasons)
	}
}

func TestIngest_Reindex_InPlaceCapable_Succeeds(t *testing.T) {
	dir := t.TempDir()
	path := buildNoCuesFrontSlotMKV(t, dir)

	plan, err := Ingest(context.Background(), path, IngestOptions{Target: "safari", Reindex: true})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Reindexed {
		t.Fatalf("expected Reindexed true (reasons=%v)", plan.Reasons)
	}
	if !plan.HasSeekIndex {
		t.Fatal("expected HasSeekIndex true after a successful in-place reindex")
	}
	if plan.NeedsReindex {
		t.Fatal("expected NeedsReindex false after a successful reindex")
	}
	if !plan.ReindexInPlacePossible {
		t.Fatal("expected ReindexInPlacePossible true after a successful in-place reindex")
	}
}

// TestIngest_Reindex_NoSeekHeadButVoidSlot_Succeeds: a source with no SeekHead
// but a Void slot in its head CAN be patched in place - the rebuilt index lands
// before the first Cluster, where any head-only reader walks past it. This used
// to be refused (ErrIndexNotHeadDiscoverable) for a reason that was never about
// the file: the metadata reader stopped at Info+Tracks and never looked at what
// followed, so it declared its own blind spot a defect of the layout. These files
// no longer pay for a full copy rewrite.
func TestIngest_Reindex_NoSeekHeadButVoidSlot_Succeeds(t *testing.T) {
	dir := t.TempDir()
	path := buildNoSeekHeadNoCuesMKV(t, dir) // no SeekHead, one Void slot after Info/Tracks

	plan, err := Ingest(context.Background(), path, IngestOptions{Target: "safari", Reindex: true})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !plan.Reindexed || !plan.HasSeekIndex || plan.NeedsReindex {
		t.Fatalf("expected an in-place reindex to succeed: reindexed=%v hasIndex=%v needsReindex=%v (reasons=%v)",
			plan.Reindexed, plan.HasSeekIndex, plan.NeedsReindex, plan.Reasons)
	}
	// And the index must really be reachable from the head, with no help from the
	// EOF fallback - that is what "in place" bought us.
	head, err := reader.OpenMeta(context.Background(), path, reader.WithCues(), reader.WithoutTailScan())
	if err != nil {
		t.Fatalf("head-only reopen: %v", err)
	}
	if len(head.Cues) == 0 {
		t.Error("the patched index is not reachable head-only without the tail scan")
	}
}

// TestIngest_Reindex_NotHeadDiscoverable_PlanStillReturned: a layout with no
// SeekHead AND no Void anywhere has nowhere to put a head-discoverable index in
// place. Ingest must not fail on it - it returns the plan and points at the copy
// reindex.
func TestIngest_Reindex_NotHeadDiscoverable_PlanStillReturned(t *testing.T) {
	dir := t.TempDir()
	path := buildNoSlotMKV(t, dir, "noslot.mkv") // no SeekHead, no Void: nothing to patch into

	plan, err := Ingest(context.Background(), path, IngestOptions{Target: "safari", Reindex: true})
	if err != nil {
		t.Fatalf("Ingest must still return a plan when in-place reindex is impossible, got error: %v", err)
	}
	if plan.Reindexed {
		t.Fatal("expected Reindexed false: the in-place patch cannot produce a head-discoverable index for this layout")
	}
	if plan.ReindexInPlacePossible {
		t.Fatal("expected ReindexInPlacePossible false for a no-front-SeekHead layout")
	}
	if !plan.NeedsReindex {
		t.Fatal("expected NeedsReindex still true")
	}
	found := false
	for _, r := range plan.Reasons {
		if strings.Contains(r, "copy reindex") {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons = %v, want a mention of falling back to a copy reindex", plan.Reasons)
	}
}

func TestIngest_HEVCMain10_Chrome_Transcode(t *testing.T) {
	dir := t.TempDir()
	w, h := uint32(1920), uint32(1080)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", Profile: "Main 10", Width: &w, Height: &h, Language: "eng", CodecPrivate: []byte{0x01}},
		audioTrack(2),
	}
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: bytes.Repeat([]byte("X"), 64)},
		{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("audio")},
	}
	path := buildMultiClusterMKV(t, dir, "hevc10.mkv", tracks, [][]mkv.Block{blocks}, 2000)

	plan, err := Ingest(context.Background(), path, IngestOptions{Target: "chrome"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Strategy != StrategyTranscode {
		t.Fatalf("strategy = %q, want transcode (reasons=%v)", plan.Strategy, plan.Reasons)
	}
	if len(plan.Ladder) == 0 {
		t.Fatal("expected a non-empty recommended ladder for a transcode plan")
	}
}

func TestIngest_IncludeAnalysis(t *testing.T) {
	plan, err := Ingest(context.Background(), sampleMKV, IngestOptions{Target: "mse-generic", IncludeAnalysis: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Analysis == nil {
		t.Fatal("expected Analysis populated when IncludeAnalysis is set")
	}
	if plan.Analysis.BlockCount <= 0 {
		t.Fatalf("Analysis.BlockCount = %d, want > 0", plan.Analysis.BlockCount)
	}
	total := int64(0)
	for _, ts := range plan.Analysis.Tracks {
		total += ts.Frames
	}
	if total <= 0 {
		t.Fatalf("total frames across tracks = %d, want > 0", total)
	}
}

func TestIngest_NoAnalysis_ByDefault(t *testing.T) {
	plan, err := Ingest(context.Background(), sampleMKV, IngestOptions{Target: "mse-generic"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Analysis != nil {
		t.Fatal("expected Analysis nil when IncludeAnalysis is not set")
	}
}

func TestIngest_UnknownTarget_Errors(t *testing.T) {
	_, err := Ingest(context.Background(), sampleMKV, IngestOptions{Target: "nonexistent-target"})
	if err == nil {
		t.Fatal("expected an error for an unknown target")
	}
}

func TestIngest_MemFS(t *testing.T) {
	data, err := os.ReadFile(sampleMKV)
	if err != nil {
		t.Fatal(err)
	}
	mem := mkv.NewMemFS()
	mem.Put("video.mkv", data)

	plan, err := Ingest(context.Background(), "video.mkv", IngestOptions{
		Options: mkv.Options{FS: mem.FS()},
		Target:  "mse-generic",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Strategy != StrategyRemuxHLS {
		t.Fatalf("strategy = %q, want remux-hls via MemFS", plan.Strategy)
	}
}

func TestIngest_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Ingest(ctx, sampleMKV, IngestOptions{Target: "mse-generic"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestIngest_ReasonsTrail_MentionsDecisiveFactor(t *testing.T) {
	dir := t.TempDir()
	dstMP4 := filepath.Join(dir, "out.mp4")
	if err := mp4.RemuxToMP4(context.Background(), sampleMKV, dstMP4); err != nil {
		t.Fatal(err)
	}

	directPlan, err := Ingest(context.Background(), dstMP4, IngestOptions{Target: "mse-generic"})
	if err != nil {
		t.Fatal(err)
	}
	if len(directPlan.Reasons) == 0 {
		t.Fatal("direct-play: expected a non-empty reasons trail")
	}
	if !strings.Contains(strings.Join(directPlan.Reasons, " "), "direct-play") {
		t.Fatalf("direct-play reasons = %v, want a mention of direct-play", directPlan.Reasons)
	}

	remuxPlan, err := Ingest(context.Background(), sampleMKV, IngestOptions{Target: "mse-generic"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(remuxPlan.Reasons, " "), "remux") {
		t.Fatalf("remux reasons = %v, want a mention of remux", remuxPlan.Reasons)
	}

	w, h := uint32(1920), uint32(1080)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", Profile: "Main 10", Width: &w, Height: &h, Language: "eng", CodecPrivate: []byte{0x01}},
	}
	blocks := []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: bytes.Repeat([]byte("X"), 64)}}
	path := buildMultiClusterMKV(t, dir, "hevc10b.mkv", tracks, [][]mkv.Block{blocks}, 1000)
	transcodePlan, err := Ingest(context.Background(), path, IngestOptions{Target: "chrome"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(transcodePlan.Reasons, " "), "transcode") {
		t.Fatalf("transcode reasons = %v, want a mention of transcode", transcodePlan.Reasons)
	}
}
