package reader

import (
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
)

// keyframes.go - derive the keyframe index from the Cues seek index. The Cues are
// read head-only (a SeekHead jump to one element, no Cluster scan) during the
// metadata pass; this turns them into Container.Keyframes so a caller gets the
// keyframe timestamps from the same OpenMeta/Read it already does.

// scaleCueTimesToMs converts the parsed cue points' raw CueTime values, which the
// file stores in TimecodeScale units, to the milliseconds mkv.CuePoint.TimeMs is
// declared to hold - and applies the Matroska default scale when the file
// declared none, so no consumer has to guard a zero.
//
// It must be called EXACTLY ONCE per container, at the point where the Cues are
// final (a second call would scale them again). Handing back raw ticks made
// every consumer wrong on a file whose scale is not one millisecond: they all
// compare TimeMs against milliseconds from elsewhere - a 30s seek-gap threshold,
// a track's end, an HLS segment length, a requested thumbnail time.
func scaleCueTimesToMs(c *mkv.Container) {
	if c.Info.TimecodeScale <= 0 {
		c.Info.TimecodeScale = mkv.DefaultTimecodeScale
	}
	if c.Info.TimecodeScale == mkv.DefaultTimecodeScale {
		return // one tick is one millisecond: the stored value already is TimeMs
	}
	for i := range c.Cues {
		c.Cues[i].TimeMs = c.Cues[i].TimeMs * c.Info.TimecodeScale / mkv.DefaultTimecodeScale
	}
}

// keyframeTimesMs converts the parsed Cues to ascending, de-duplicated millisecond
// timestamps, or nil when there are none. Cue times are already milliseconds by
// the time this runs (see scaleCueTimesToMs). When a video track is identifiable
// and the Cues reference it, only its cue points are used (audio-only cue points
// are dropped); otherwise every cue point is used.
func keyframeTimesMs(c *mkv.Container) []int64 {
	if len(c.Cues) == 0 {
		return nil
	}
	var videoTrack uint64
	for i := range c.Tracks {
		if c.Tracks[i].Type == mkv.VideoTrack {
			videoTrack = c.Tracks[i].ID
			break
		}
	}
	times := make([]int64, 0, len(c.Cues))
	for _, cue := range c.Cues {
		if videoTrack != 0 && cue.Track != 0 && cue.Track != videoTrack {
			continue
		}
		times = append(times, cue.TimeMs)
	}
	// If filtering by the video track dropped everything (e.g. the Cues index a
	// different track number), fall back to every cue point.
	if len(times) == 0 {
		for _, cue := range c.Cues {
			times = append(times, cue.TimeMs)
		}
	}
	sort.Slice(times, func(a, b int) bool { return times[a] < times[b] })
	out := times[:1]
	for _, t := range times[1:] {
		if t != out[len(out)-1] {
			out = append(out, t)
		}
	}
	return out
}
