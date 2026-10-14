package mp4

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildSparseCueMKV writes ~1 s clusters, each opening on a keyframe, and cues
// only those whose index uncued leaves false: the Cues skip keyframes.
func buildSparseCueMKV(t testing.TB, tracks []mkv.Track, blocks []genBlock, uncued map[int]bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sparse.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const scale = 1_000_000
	var blks []mkv.Block
	var durationMs int64
	for _, gb := range blocks {
		blks = append(blks, mkv.Block{TrackNumber: gb.track, Timecode: gb.pts, Keyframe: gb.key, Data: gb.data})
		durationMs = max(durationMs, gb.pts)
	}
	m := writer.NewMKVWriter(f)
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, durationMs); err != nil {
		t.Fatal(err)
	}
	opens := func(b, first mkv.Block) bool { return b.TrackNumber == 1 && b.Keyframe && b.Timecode > first.Timecode }
	start, k := 0, 0
	for i := 1; i <= len(blks); i++ {
		if i < len(blks) && !opens(blks[i], blks[start]) {
			continue
		}
		if uncued[k] {
			err = writer.WriteCluster(f, blks[start].Timecode, scale, blks[start:i])
		} else {
			err = m.WriteClusterWithCues(blks[start].Timecode, scale, blks[start:i])
		}
		if err != nil {
			t.Fatal(err)
		}
		start, k = i, k+1
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPlanHLSCueDensity: a Matroska plan reports how densely its Cues index the
// video keyframes it cuts on, so a caller can tell a plan that matches the full
// pass from one that cannot and synthesize the index itself.
func TestPlanHLSCueDensity(t *testing.T) {
	ctx := context.Background()
	tracks, blocks := synthFixtureTracks(), synthFixtureBlocks(0) // keyframes at 0, 1000, ... 5000 ms

	sparse := buildSparseCueMKV(t, tracks, blocks, map[int]bool{2: true, 5: true})
	plan, err := PlanHLS(ctx, sparse, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	d, ok := plan.CueDensity()
	want := CueDensity{CuePoints: 4, MaxGapMs: 2000, MaxGapAtMs: 1000, MedianGapMs: 1000, LastCueMs: 4000}
	if !ok || d != want {
		t.Errorf("sparse Cues: CueDensity() = %+v, %v; want %+v, true", d, ok, want)
	}

	synth, err := PlanHLS(ctx, buildNoCueMKV(t, tracks, blocks), Options{SegmentMs: 1000, SynthesizeIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	d, ok = synth.CueDensity()
	want = CueDensity{CuePoints: 6, MaxGapMs: 1000, MedianGapMs: 1000, LastCueMs: 5000, Synthesized: true}
	if !ok || d != want {
		t.Errorf("synthesized index: CueDensity() = %+v, %v; want %+v, true", d, ok, want)
	}

	mp4Plan, err := PlanHLS(ctx, buildTestMP4(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mp4Plan.CueDensity(); ok {
		t.Error("an MP4 plan cuts on its sample table, not on Cues: CueDensity() must report ok = false")
	}
}
