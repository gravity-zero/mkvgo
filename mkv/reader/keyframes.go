package reader

import (
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
)

// keyframes.go — derive the keyframe index from the Cues seek index. The Cues are
// read head-only (a SeekHead jump to one element, no Cluster scan) during the
// metadata pass; this turns them into Container.Keyframes so a caller gets the
// keyframe timestamps from the same OpenMeta/Read it already does.

// keyframeTimesMs converts the parsed Cues to ascending, de-duplicated millisecond
// timestamps, or nil when there are none. CueTime is in TimecodeScale units and is
// scaled to ms. When a video track is identifiable and the Cues reference it, only
// its cue points are used (audio-only cue points are dropped); otherwise every cue
// point is used.
func keyframeTimesMs(c *mkv.Container) []int64 {
	if len(c.Cues) == 0 {
		return nil
	}
	scale := c.Info.TimecodeScale
	if scale <= 0 {
		scale = 1_000_000 // default 1 ms per tick
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
		times = append(times, cue.TimeMs*scale/1_000_000)
	}
	// If filtering by the video track dropped everything (e.g. the Cues index a
	// different track number), fall back to every cue point.
	if len(times) == 0 {
		for _, cue := range c.Cues {
			times = append(times, cue.TimeMs*scale/1_000_000)
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
