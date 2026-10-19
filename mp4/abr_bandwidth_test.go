package mp4

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

var abrBandwidth = regexp.MustCompile(`#EXT-X-STREAM-INF:BANDWIDTH=(\d+)`)

func abrBandwidths(t *testing.T, master []byte) []int64 {
	t.Helper()
	var bws []int64
	for _, m := range abrBandwidth.FindAllSubmatch(master, -1) {
		v, _ := strconv.ParseInt(string(m[1]), 10, 64)
		bws = append(bws, v)
	}
	if len(bws) != 2 {
		t.Fatalf("master declares %d variants, want 2:\n%s", len(bws), master)
	}
	return bws
}

// TestABRVideoOnlyVariantCountsSharedAudio: a video-only variant plays the reference's audio, yet its BANDWIDTH
// counted its video alone, so the same source declared less as v2 than as v1.
func TestABRVideoOnlyVariantCountsSharedAudio(t *testing.T) {
	src, _ := buildLacedFixture(t)
	mp4Src := filepath.Join(t.TempDir(), "src.mp4")
	if err := RemuxToMP4(context.Background(), src, mp4Src); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{src, mp4Src} {
		dir := t.TempDir()
		if err := RemuxToABR(context.Background(), []string{s, s}, dir, Options{SegmentMs: 2000}); err != nil {
			t.Fatal(err)
		}
		master, err := os.ReadFile(filepath.Join(dir, "master.m3u8"))
		if err != nil {
			t.Fatal(err)
		}
		if bw := abrBandwidths(t, master); bw[1] < bw[0] {
			t.Errorf("%s full pass: v2 BANDWIDTH %d below v1 %d for the same source", filepath.Ext(s), bw[1], bw[0])
		}
	}
	plan, err := PlanABR(context.Background(), []string{mp4Src, mp4Src}, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if bw := abrBandwidths(t, plan.MasterPlaylist()); bw[1] < bw[0] {
		t.Errorf("MP4 plan: v2 BANDWIDTH %d below v1 %d for the same source", bw[1], bw[0])
	}
}
