package mp4

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildLongCueFixture writes 400 s of video with a cue that outlasts subFastMaxCueDurMs, starting at 96 s.
func buildLongCueFixture(t *testing.T) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt", Language: "eng"},
	}
	frame := append([]byte{0x00, 0x00, 0x00, 0x01, 0x65}, make([]byte, 1<<10)...)
	var blks []mkv.Block
	for i := 0; i < 2000; i++ {
		blks = append(blks, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 200, Keyframe: i%5 == 0, Data: frame})
	}
	for _, b := range []mkv.Block{
		{TrackNumber: 2, Timecode: 1000, Duration: 2000, Data: []byte("head")},
		{TrackNumber: 2, Timecode: 96_000, Duration: 200_000, Data: []byte("the long cue")},
	} {
		b.Keyframe = true
		blks = append(blks, b)
	}
	for i := 1; i < len(blks); i++ {
		for j := i; j > 0 && blks[j].Timecode < blks[j-1].Timecode; j-- {
			blks[j], blks[j-1] = blks[j-1], blks[j]
		}
	}
	path := filepath.Join(t.TempDir(), "long.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const scale = 1_000_000
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, 400_000); err != nil {
		t.Fatal(err)
	}
	if err := writeTestClusters(m, scale, blks); err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// A harvested scan stops where the walk's strides stop: a scan cut short at the request's own reach left a cue longer
// than the island margin unseen, and the jump that followed served an island that never reads it.
func TestSubtitleHarvestKeepsTheWalkStrides(t *testing.T) {
	ctx := context.Background()
	src := buildLongCueFixture(t)
	walked, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	walked.harvest.off = true
	harvested, err := PlanHLS(ctx, src, Options{SegmentMs: 2000, SourceAccess: SourceLocal})
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range []string{"sub1_00019.vtt", "sub1_00097.vtt"} {
		want, _, werr := walked.Resource(ctx, res)
		got, _, gerr := harvested.Resource(ctx, res)
		if werr != nil || gerr != nil {
			t.Fatalf("%s: walked %v, harvested %v", res, werr, gerr)
		}
		if res == "sub1_00097.vtt" && !bytes.Contains(want, []byte("the long cue")) {
			t.Fatalf("the walk no longer serves the long cue at 192 s: the fixture misses the case\n%s", want)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s differs when harvested:\n%s\n--- walked:\n%s", res, got, want)
		}
	}
	if harvested.Stats().SubtitleHarvestedClusters == 0 {
		t.Fatal("nothing harvested")
	}
}

// The media segments are the same bytes whether the walks read the subtitles along or not.
func TestSubtitleHarvestLeavesTheMediaAlone(t *testing.T) {
	ctx := context.Background()
	for _, build := range []func(testing.TB) string{buildSeekFixture, buildSubCursorFixture} {
		src := build(t)
		for _, access := range []SourceAccess{SourceAuto, SourceLocal, SourceNetwork} {
			o := Options{SegmentMs: 2000, SourceAccess: access}
			plain, err := PlanHLS(ctx, src, o)
			if err != nil {
				t.Fatal(err)
			}
			plain.harvest.off = true
			harvested, err := PlanHLS(ctx, src, o)
			if err != nil {
				t.Fatal(err)
			}
			for n := 0; n < harvested.NumSegments(); n++ {
				if n%7 == 3 { // a subtitle asked first: its windows are walked for it
					if _, _, err := harvested.Resource(ctx, fmt.Sprintf("sub1_%05d.vtt", n+1)); err != nil {
						t.Fatal(err)
					}
				}
				want, _, werr := plain.Resource(ctx, plain.SegmentName(n))
				got, _, gerr := harvested.Resource(ctx, harvested.SegmentName(n))
				if werr != nil || gerr != nil || !bytes.Equal(got, want) {
					t.Fatalf("segment %d differs (access %v): %v, %v", n, access, werr, gerr)
				}
			}
		}
	}
}

// A subtitle request builds no window longer than the walk's stride: a last window running over minutes the Cues
// leave out read the rest of the file for a subtitle the walk serves from a stride.
func TestSubtitleHarvestBuildsNoWindowLongerThanAStride(t *testing.T) {
	ctx := context.Background()
	src := buildSeekFixture(t)
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2 * subScanStrideMs, SourceAccess: SourceLocal})
	if err != nil {
		t.Fatal(err)
	}
	if plan.windowMs(0) <= subScanStrideMs {
		t.Fatalf("window 0 lasts %d ms: the fixture no longer has windows longer than a stride", plan.windowMs(0))
	}
	if _, _, err := plan.Resource(ctx, "sub1_00001.vtt"); err != nil {
		t.Fatal(err)
	}
	if st := plan.Stats(); st.TableBuilds != 0 || st.SubtitleWalks == 0 {
		t.Errorf("%d tables built, %d walks: want the walk alone", st.TableBuilds, st.SubtitleWalks)
	}
}

// Nor the last window, whose walk runs to the end of the file: a file holding minutes past its Cues and its
// declared duration (a real episode: 2.6 GB past a last window declared 6.6 s long) read them all for one subtitle.
func TestSubtitleHarvestBuildsNotTheLastWindow(t *testing.T) {
	ctx := context.Background()
	plan, err := PlanHLS(ctx, buildSeekFixture(t), Options{SegmentMs: 2000, SourceAccess: SourceLocal})
	if err != nil {
		t.Fatal(err)
	}
	last := plan.segCount - 1
	if plan.harvestWindow(ctx, plan.offsets[last], map[int]bool{last - 1: true}) {
		t.Error("the last window was built for a subtitle")
	}
	if n := plan.Stats().TableBuilds; n != 0 {
		t.Errorf("%d tables built", n)
	}
}
