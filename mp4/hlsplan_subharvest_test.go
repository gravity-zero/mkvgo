package mp4_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/gravity-zero/mkvgo/mp4"
)

func openAll(t *testing.T, plan *mp4.HLSPlan, name string) []byte {
	t.Helper()
	ctx := context.Background()
	h, err := plan.Open(ctx, name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	defer h.Close()
	var buf bytes.Buffer
	if _, err := h.WriteTo(ctx, &buf); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return buf.Bytes()
}

// The subtitle renditions take their cues from the clusters the window walks read, and serve the very WebVTT the
// cluster walk serves: every segment and whole-track file, in any order, whether the video was asked for before,
// after or not at all.
func TestPlanHLSSubtitleHarvestServesTheSameWebVTT(t *testing.T) {
	ctx := context.Background()
	videoFirst := map[string]func(plan *mp4.HLSPlan, n int) []int{
		"no video":           func(*mp4.HLSPlan, int) []int { return nil },
		"the video before":   func(_ *mp4.HLSPlan, n int) []int { return []int{n - 1} },
		"every other window": func(_ *mp4.HLSPlan, n int) []int { return []int{(n - 1) &^ 1} },
	}
	for name, build := range map[string]func(testing.TB) string{
		"cues with durations":           mp4.BuildSeekFixtureForTest,
		"cues that end on the next cue": mp4.BuildSubCursorFixtureForTest,
	} {
		src := build(t)
		for orderName, order := range subOrders {
			for vName, video := range videoFirst {
				for _, access := range []mp4.SourceAccess{mp4.SourceLocal, mp4.SourceNetwork} {
					t.Run(fmt.Sprintf("%s/%s/%s/%v", name, orderName, vName, access), func(t *testing.T) {
						walked, err := mp4.PlanHLS(ctx, src, mp4.Options{SegmentMs: 2000})
						if err != nil {
							t.Fatal(err)
						}
						mp4.DisableSubHarvestForTest(walked)
						harvested, err := mp4.PlanHLS(ctx, src, mp4.Options{SegmentMs: 2000, SourceAccess: access})
						if err != nil {
							t.Fatal(err)
						}
						for _, res := range subNames(walked, order) {
							var n int
							if _, err := fmt.Sscanf(res[len("subX_"):], "%05d.vtt", &n); err == nil {
								for _, v := range video(harvested, n) {
									if v >= 0 && v < harvested.NumSegments() {
										openAll(t, harvested, harvested.SegmentName(v))
									}
								}
							}
							want, _, werr := walked.Resource(ctx, res)
							got, _, gerr := harvested.Resource(ctx, res)
							if werr != nil || gerr != nil {
								t.Fatalf("%s: walked %v, harvested %v", res, werr, gerr)
							}
							if !bytes.Equal(got, want) {
								t.Fatalf("%s differs when harvested:\n%s\n--- walked:\n%s", res, got, want)
							}
						}
						if st := harvested.Stats(); st.SubtitleHarvestedClusters == 0 && orderName == "playback order" { // asked behind what was read, a subtitle walks
							t.Errorf("no cluster harvested: %+v", st)
						}
						if st := walked.Stats(); st.SubtitleHarvestedClusters != 0 || st.SubtitleWalks == 0 {
							t.Errorf("the reference harvested: %+v", st)
						}
					})
				}
			}
		}
	}
}

// The point of the harvest: a viewer playing the video with a subtitle reads the source once, as without the
// subtitle, instead of once more per subtitle rendition.
func TestPlanHLSSubtitleHarvestReadsTheSourceOnce(t *testing.T) {
	ctx := context.Background()
	src := mp4.BuildSeekFixtureForTest(t)
	play := func(subs int, harvest bool) mp4.HLSPlanStats {
		plan, err := mp4.PlanHLS(ctx, src, mp4.Options{SegmentMs: 2000, SourceAccess: mp4.SourceLocal})
		if err != nil {
			t.Fatal(err)
		}
		if !harvest {
			mp4.DisableSubHarvestForTest(plan)
		}
		for n := 0; n < plan.NumSegments(); n++ { // the whole file: a subtitle reads up to a stride ahead of the video, which the end evens out
			openAll(t, plan, plan.SegmentName(n))
			for k := 1; k <= subs; k++ {
				if _, _, err := plan.Resource(ctx, fmt.Sprintf("sub%d_%05d.vtt", k, n+1)); err != nil {
					t.Fatal(err)
				}
			}
		}
		return plan.Stats()
	}
	alone, walked, harvested := play(0, true), play(2, false), play(2, true)
	if walked.WalkReadBytes < 2*alone.WalkReadBytes {
		t.Fatalf("the walked subtitles read %d bytes, the video alone %d: the fixture no longer shows the second walk", walked.WalkReadBytes, alone.WalkReadBytes)
	}
	if harvested.SubtitleHarvestedClusters == 0 || harvested.WalkReadBytes*100 > alone.WalkReadBytes*102 { // the last window's clusters are walked
		t.Errorf("harvested: %d walks, %d bytes walked; the video alone %d, walked subtitles %d", harvested.SubtitleWalks, harvested.WalkReadBytes, alone.WalkReadBytes, walked.WalkReadBytes)
	}
}
