package mp4

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// buildTailPlanSource builds a small PlanHLS-compatible source (one video, one
// audio track) and returns its path.
func buildTailPlanSource(t *testing.T) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr := 48000.0
	ch := uint8(2)
	var blocks []genBlock
	for i := 0; i < 30; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%15 == 0, data: cencVideoSample()})
		blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 40, key: true, data: cencAudioSample(i)})
	}
	sortGenBlocks(blocks)
	return buildPlanFixture(t,
		[]mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
		},
		blocks, nil)
}

// TestPlanABRVariantErrorWrap pins the per-variant error wrap in PlanABR
// (abrplan.go): when a non-reference source fails to plan, the error is wrapped
// with its 1-based variant number. This path is reached only on a failing
// variant, which no prior test exercised (gremlins reported the line as not
// covered). Asserting the "variant 2" prefix kills the i+1 arithmetic mutant.
func TestPlanABRVariantErrorWrap(t *testing.T) {
	good := buildTailPlanSource(t)
	bad := filepath.Join(t.TempDir(), "does-not-exist.mkv")
	_, err := PlanABR(context.Background(), []string{good, bad}, Options{SegmentMs: 2000})
	if err == nil {
		t.Fatal("PlanABR with a missing second source should fail")
	}
	if !strings.Contains(err.Error(), "variant 2") {
		t.Errorf("error should name the failing variant (2), got %q", err.Error())
	}
}
