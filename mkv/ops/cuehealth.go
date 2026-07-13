package ops

import (
	"context"
	"fmt"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// cuehealth.go - CueHealth classifies a file's seek index HEAD-ONLY. Validate
// proves cue times against real keyframes but walks the whole file; CueHealth
// answers the cheaper, earlier question a library scan needs - "can this index
// seek video at all?" - from the Tracks and Cues alone, in milliseconds. The
// classic defect it surfaces: a muxer that cued the audio instead of the video,
// so a video file's index exists, readers call the file "indexed", and every
// seek still lands mid-GOP.
//
// The verdict judges the VIDEO cues alone, because they are what seeking uses:
// the keyframe index is built from the video-keyed cues and drops the rest
// (reader.keyframeTimesMs). Cues on an audio track are therefore inert - a real
// muxer that cues every track produces an index that is mostly "non-video" and
// seeks perfectly - so their share is reported (NonVideoPct) but never condemns
// a file on its own. What condemns a file is a video index that cannot serve a
// seek: none at all (misskeyed), or holes too wide to land in (sparse).

// CueHealthReport classifies a file's CuePoints by referenced track. The
// definition lives in the mkv package (see mkv/reports.go).
type CueHealthReport = mkv.CueHealthReport

// CueHealth reads path's Tracks and Cues head-only (through the SeekHead, or a
// bounded read back from EOF when nothing indexes the Cues - never a cluster
// walk), classifies every CuePoint by the track it references, and judges the
// video cues' coverage. It is the scan-time complement of Validate: Validate
// proves cue times against real keyframes at the cost of a full read; CueHealth
// spots an index that cannot seek video - absent, keyed on the wrong tracks, or
// too full of holes - in milliseconds.
func CueHealth(ctx context.Context, path string, opts ...mkv.Options) (*CueHealthReport, error) {
	fs := mkv.FSFrom(opts)
	meta, err := reader.OpenMetaWithFS(ctx, path, fs, reader.WithCues())
	if err != nil {
		return nil, fmt.Errorf("cue health: %w", err)
	}

	known := make(map[uint64]bool, len(meta.Tracks))
	video := videoTrackSet(meta.Tracks)
	for _, t := range meta.Tracks {
		known[t.ID] = true
	}

	r := &CueHealthReport{
		PerTrack:      map[uint64]int{},
		HasVideoTrack: len(video) > 0,
	}
	for i, c := range meta.Cues {
		r.TotalCues++
		r.PerTrack[c.Track]++
		switch {
		case video[c.Track]:
			r.VideoCues++
		case !known[c.Track]:
			r.UnknownTrackCues++
		default:
			r.NonVideoCues++
		}
		if i == 0 || c.TimeMs < r.FirstCueMs {
			r.FirstCueMs = c.TimeMs
		}
		if c.TimeMs > r.LastCueMs {
			r.LastCueMs = c.TimeMs
		}
	}
	if r.TotalCues > 0 {
		r.NonVideoPct = float64(r.NonVideoCues+r.UnknownTrackCues) * 100 / float64(r.TotalCues)
	}
	if r.HasVideoTrack {
		r.MaxVideoGapMs = maxVideoGapMs(meta, video)
	}

	switch {
	case r.TotalCues == 0:
		r.Reason = "no seek index: run mkvgo reindex"
	case r.UnknownTrackCues > 0:
		r.Reason = fmt.Sprintf("%d cue(s) reference tracks that do not exist (stale index): run mkvgo reindex", r.UnknownTrackCues)
	case r.HasVideoTrack && r.VideoCues == 0:
		r.Reason = fmt.Sprintf("the index keys on non-video tracks only (%d cue(s), no video cue) - every seek lands mid-GOP: run mkvgo reindex", r.TotalCues)
	case r.HasVideoTrack && r.MaxVideoGapMs > maxSeekGapMs:
		r.Reason = fmt.Sprintf("the video cues leave a %.0fs hole - a seek there lands that far from its target: run mkvgo reindex",
			float64(r.MaxVideoGapMs)/1000)
	default:
		r.Healthy = true
	}
	return r, nil
}

// maxSeekGapMs is the widest hole tolerated in a file's video cue coverage. Past
// it, a seek into the hole lands more than this far from its target, which is no
// longer seeking - and a reindex fixes it, writing one cue per cluster that
// holds a video keyframe (a few seconds apart on any real mux). Below it, the
// index does its job however many cues sit on other tracks.
const maxSeekGapMs = 30_000

// maxVideoGapMs measures the worst hole in the video cue coverage: between
// consecutive video cues, and - since a seek before the first or after the last
// one lands on that cue - from 0 to the first and from the last to the duration.
// The tail gap is only counted when the duration is known and past the last cue,
// so a file that declares none is judged on its cue spacing alone.
func maxVideoGapMs(c *mkv.Container, video map[uint64]bool) int64 {
	times := make([]int64, 0, len(c.Cues))
	for _, cue := range c.Cues {
		if video[cue.Track] {
			times = append(times, cue.TimeMs)
		}
	}
	if len(times) == 0 {
		return 0 // no video cue at all: the misskeyed rule speaks, not this one
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })

	worst := times[0] // the head gap: a seek before the first cue lands on it
	for i := 1; i < len(times); i++ {
		if gap := times[i] - times[i-1]; gap > worst {
			worst = gap
		}
	}
	if last := times[len(times)-1]; c.DurationMs > last {
		if gap := c.DurationMs - last; gap > worst {
			worst = gap
		}
	}
	return worst
}
