package ops

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// cuehealth.go - CueHealth classifies a file's seek index HEAD-ONLY: which
// tracks its CuePoints reference. Validate proves cue times against real
// keyframes but walks the whole file; CueHealth answers the cheaper, earlier
// question a library scan needs - "is this index even pointing at the right
// tracks?" - from the SeekHead, Tracks and Cues alone, in milliseconds. The
// classic defect it surfaces: a muxer that cued every track (or the audio),
// so a video file's index exists, readers call the file "indexed", and every
// seek still lands mid-GOP.

// CueHealthReport classifies a file's CuePoints by referenced track. The
// definition lives in the mkv package (see mkv/reports.go).
type CueHealthReport = mkv.CueHealthReport

// CueHealth reads path's Tracks and Cues head-only (SeekHead-guided, no
// cluster walk) and classifies every CuePoint by the track it references.
// It is the scan-time complement of Validate: Validate proves cue times
// against real keyframes at the cost of a full read; CueHealth spots an
// index keyed on the wrong tracks - present, non-empty, and useless for
// seeking - in milliseconds.
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

	switch {
	case r.TotalCues == 0:
		r.Reason = "no seek index: run mkvgo reindex"
	case r.UnknownTrackCues > 0:
		r.Reason = fmt.Sprintf("%d cue(s) reference tracks that do not exist (stale index): run mkvgo reindex", r.UnknownTrackCues)
	case r.HasVideoTrack && r.NonVideoCues > 0:
		r.Reason = fmt.Sprintf("%.0f%% of cues reference non-video tracks - seeking lands mid-GOP: run mkvgo reindex", r.NonVideoPct)
	default:
		r.Healthy = true
	}
	return r, nil
}
