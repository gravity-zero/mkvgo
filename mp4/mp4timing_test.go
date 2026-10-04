package mp4

import (
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// fieldPairSamples is the opening of an interlaced stream stored one field per
// sample: the first two fields carry the same presentation time, then the
// times move on in decode order - exactly the shape of a collapsed audio lace
// (a run of equal timecodes, then a larger one).
func fieldPairSamples() []fragSample {
	pts := []int64{0, 0, 133, 150, 33, 50, 67, 83, 100, 117, 200, 217, 167, 183}
	samples := make([]fragSample, len(pts))
	for i, p := range pts {
		samples[i] = fragSample{size: 100, ptsMs: p, blockPtsMs: p, sync: i == 0}
	}
	return samples
}

// Two video fields stamped with the same time are not a collapsed audio lace:
// a video track must never be put on the grid deriveGridTS recovers from one
// (67 ms a field here - a 59 s clip came out 238 s long).
func TestVideoIsNeverGridTimed(t *testing.T) {
	video := &outTrack{mkv: mkv.Track{Type: mkv.VideoTrack}}
	if got := audioGridTS(video, movieTimescale); got != noGridTS {
		t.Fatalf("audioGridTS(video) = %d, want noGridTS", got)
	}

	samples := fieldPairSamples()
	_, _, total, _ := fillFragTiming(samples, 0, movieTimescale, audioGridTS(video, movieTimescale))
	if total > 300 {
		t.Errorf("total duration = %d ms for fields spanning 217 ms: the video was grid-timed", total)
	}
	for i := 1; i < len(samples); i++ {
		if samples[i].durTS == 67 && samples[i-1].durTS == 67 {
			t.Fatalf("samples %d and %d both last 67 ms: the collapsed-lace stride was applied to video", i-1, i)
		}
	}

	window := fieldPairSamples()
	pt := &planTrack{ft: &fragTrack{outTrack: video, timescale: movieTimescale}, gridTS: noGridTS}
	timeSegmentWindow(window, pt, 233)
	var windowTotal int64
	for i := range window {
		windowTotal += window[i].durTS
	}
	if windowTotal > 300 {
		t.Errorf("on-demand window lasts %d ms for fields spanning 233 ms: the video was grid-timed", windowTotal)
	}
}
