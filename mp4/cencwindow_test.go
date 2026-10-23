package mp4

import (
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestWindowReachingTheNextKeepsItsIVs pins that a window whose decode times reach the next window's first is timed
// forward and refused under 'cenc', whose IVs the next segment would repeat.
func TestWindowReachingTheNextKeepsItsIVs(t *testing.T) {
	video := &outTrack{mkv: mkv.Track{Type: mkv.VideoTrack}}
	for _, pts := range [][]int64{{960, 1000}, {998, 999, 999, 999}} {
		window := make([]fragSample, len(pts))
		for i, p := range pts {
			window[i] = fragSample{size: 1, ptsMs: p, blockPtsMs: p, sync: i == 0}
		}
		pt := &planTrack{ft: &fragTrack{outTrack: video, timescale: movieTimescale}, gridTS: noGridTS}
		timeSegmentWindow(window, pt, 1000, -1)
		for i, s := range window {
			if s.durTS <= 0 {
				t.Errorf("%v: sample %d lasts %d ticks", pts, i, s.durTS)
			}
		}
		c := &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIVFor("cenc"), InlineKey: true}
		_, _, err := prepareCENCSegment(c, false, "aac", nil, 1, window, make([]byte, len(window)))
		if err == nil || !strings.Contains(err.Error(), "cbcs") {
			t.Errorf("%v: 'cenc' over a window reaching the next: err = %v, want a refusal naming cbcs", pts, err)
		}
		cbcs := &CENCOptions{Scheme: "cbcs", Key: cencKey, KeyID: cencKID, IV: cencIVFor("cbcs"), InlineKey: true}
		if _, _, err := prepareCENCSegment(cbcs, false, "aac", nil, 1, window, make([]byte, len(window))); err != nil {
			t.Errorf("%v: cbcs refused: %v", pts, err)
		}
	}
}
