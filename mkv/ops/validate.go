package ops

import (
	"context"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

func Validate(ctx context.Context, path string, opts ...mkv.Options) ([]mkv.Issue, error) {
	fs := mkv.FSFrom(opts)
	var issues []mkv.Issue

	stat, err := fs.DoStat(path)
	if err != nil {
		return nil, err
	}

	c, err := reader.OpenWithFS(ctx, path, fs)
	if err != nil {
		return nil, fmt.Errorf("cannot parse: %w", err)
	}

	if c.Info.TimecodeScale == 0 {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityError, Code: "no-timecodescale", Message: "missing TimecodeScale"})
	}
	if c.Info.Duration == 0 && c.DurationMs == 0 {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "no-duration", Message: "no duration set"})
	}
	if c.Info.MuxingApp == "" {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "no-muxing-app", Message: "missing MuxingApp"})
	}
	if c.Info.WritingApp == "" {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "no-writing-app", Message: "missing WritingApp"})
	}

	if len(c.Tracks) == 0 {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityError, Code: "no-tracks", Message: "no tracks"})
	}

	hasVideo := false
	trackIDs := map[uint64]bool{}
	for _, t := range c.Tracks {
		if trackIDs[t.ID] {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityError, Code: "duplicate-track-id", Track: t.ID, Message: fmt.Sprintf("duplicate track ID %d", t.ID)})
		}
		trackIDs[t.ID] = true

		if t.Type == mkv.VideoTrack {
			hasVideo = true
		}
		if t.Codec == "" {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityError, Code: "track-no-codec", Track: t.ID, Message: fmt.Sprintf("track %d: no codec", t.ID)})
		}
		if t.Type == mkv.VideoTrack && (t.Width == nil || t.Height == nil) {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "video-no-dimensions", Track: t.ID, Message: fmt.Sprintf("track %d: video without dimensions", t.ID)})
		}
		if t.Type == mkv.VideoTrack && len(t.CodecPrivate) == 0 {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "video-no-codecprivate", Track: t.ID, Message: fmt.Sprintf("track %d: video without CodecPrivate", t.ID)})
		}
		if t.Type == mkv.AudioTrack && t.SampleRate == nil {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "audio-no-samplerate", Track: t.ID, Message: fmt.Sprintf("track %d: audio without sample rate", t.ID)})
		}
		if t.Language == "" {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "track-no-language", Track: t.ID, Message: fmt.Sprintf("track %d: no language set", t.ID)})
		}
	}
	if !hasVideo {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "no-video-track", Message: "no video track"})
	}

	f, err := fs.DoOpen(path)
	if err != nil {
		return issues, nil
	}
	defer f.Close()

	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityError, Code: "clusters-unreadable", Message: fmt.Sprintf("cannot read clusters: %v", err)})
		return issues, nil
	}

	videoIDs := map[uint64]bool{}
	textIDs := map[uint64]bool{}
	for _, t := range c.Tracks {
		if t.Type == mkv.VideoTrack {
			videoIDs[t.ID] = true
		}
		if t.Type == mkv.SubtitleTrack {
			textIDs[t.ID] = true
		}
	}

	blockCounts := map[uint64]int{}
	videoKfPts := map[int64]bool{} // video keyframe times, to audit the Cues against
	var lastTC int64
	var blockTotal, subNoDuration int
	var hasKeyframe bool
	for {
		if ctx.Err() != nil {
			return issues, ctx.Err()
		}
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityError, Code: "cluster-read-error", Message: fmt.Sprintf("cluster read error at block %d: %v", blockTotal, err)})
			break
		}
		blockCounts[blk.TrackNumber]++
		blockTotal++
		if blk.Keyframe {
			hasKeyframe = true
			if videoIDs[blk.TrackNumber] {
				videoKfPts[blk.Timecode] = true
			}
		}
		if textIDs[blk.TrackNumber] && blk.Duration == 0 {
			subNoDuration++
		}
		if blk.Timecode < lastTC-1000 {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "timecode-backwards", Message: fmt.Sprintf("timecode went backwards: %dms → %dms at block %d", lastTC, blk.Timecode, blockTotal)})
		}
		lastTC = blk.Timecode
	}

	if blockTotal == 0 && stat.Size() > 1024 {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "no-blocks", Message: "no blocks found (metadata-only file?)"})
	}
	if !hasKeyframe && blockTotal > 0 {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "no-keyframes", Message: "no keyframes found"})
	}

	for _, t := range c.Tracks {
		if blockCounts[t.ID] == 0 && blockTotal > 0 {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "track-no-blocks", Track: t.ID, Message: fmt.Sprintf("track %d (%s): no blocks", t.ID, t.Type)})
		}
	}

	// Streaming readiness - what seeking, `hls-segment` and `extract-frame`
	// rely on: a Cues index keyed on real video keyframes.
	switch {
	case blockTotal == 0:
	case len(c.Cues) == 0:
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "no-cues",
			Message: "no Cues index - seeking, on-demand HLS and keyframe extraction need one (run `mkvgo reindex`)"})
	case len(videoIDs) > 0:
		misKeyed, videoCues, stale := 0, 0, 0
		for _, cue := range c.Cues {
			switch {
			case !videoIDs[cue.Track]:
				misKeyed++
			default:
				videoCues++
				if !videoKfPts[cue.TimeMs] {
					stale++
				}
			}
		}
		// A cue on a non-video track is INERT: the keyframe index is built from
		// the video-keyed cues and drops the rest, so seeking never lands on it.
		// It only breaks the file when it is all there is - an index with no
		// video cue cannot seek video at all. Plenty of real muxers cue every
		// track, which leaves an index that is mostly non-video and seeks fine:
		// that is bloat (a reindex slims it), not a defect.
		switch {
		case misKeyed > 0 && videoCues == 0:
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityError, Code: "cues-non-video",
				Message: fmt.Sprintf("all %d cue points reference a non-video track - no video cue to seek to, every seek lands mid-GOP (rewrite with `mkvgo reindex`)", misKeyed)})
		case misKeyed > 0:
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "cues-non-video",
				Message: fmt.Sprintf("%d/%d cue points reference a non-video track - inert for seeking (the %d video cues serve it), index bloat only (slim it with `mkvgo reindex`)", misKeyed, len(c.Cues), videoCues)})
		}
		if stale > 0 {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "cues-stale",
				Message: fmt.Sprintf("%d/%d cue times match no video keyframe - stale or rounded cue index (rewrite with `mkvgo reindex`)", stale, len(c.Cues))})
		}
	}
	if subNoDuration > 0 {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "subs-no-blockduration",
			Message: fmt.Sprintf("%d subtitle blocks carry no BlockDuration - cue end times are lost (readers fall back to guesses)", subNoDuration)})
	}
	for _, t := range c.Tracks {
		if t.Type == mkv.VideoTrack && t.FrameRate == nil && blockCounts[t.ID] > 0 {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "video-no-defaultduration", Track: t.ID,
				Message: fmt.Sprintf("track %d: video without DefaultDuration - frame rate and last-sample durations must be guessed downstream", t.ID)})
		}
		if t.Type == mkv.AudioTrack && t.Codec == "aac" && len(t.CodecPrivate) == 0 {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "aac-no-asc", Track: t.ID,
				Message: fmt.Sprintf("track %d: AAC without an AudioSpecificConfig (CodecPrivate) - remuxing to MP4/HLS needs it", t.ID)})
		}
	}
	for _, a := range c.Attachments {
		if a.MIMEType == "" {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Code: "attachment-no-mime",
				Message: fmt.Sprintf("attachment %d (%s): no MIME type", a.ID, a.Name)})
		}
	}

	return issues, nil
}
