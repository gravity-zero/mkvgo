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
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityError, Message: "missing TimecodeScale"})
	}
	if c.Info.Duration == 0 && c.DurationMs == 0 {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Message: "no duration set"})
	}
	if c.Info.MuxingApp == "" {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Message: "missing MuxingApp"})
	}
	if c.Info.WritingApp == "" {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Message: "missing WritingApp"})
	}

	if len(c.Tracks) == 0 {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityError, Message: "no tracks"})
	}

	hasVideo := false
	trackIDs := map[uint64]bool{}
	for _, t := range c.Tracks {
		if trackIDs[t.ID] {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityError, Message: fmt.Sprintf("duplicate track ID %d", t.ID)})
		}
		trackIDs[t.ID] = true

		if t.Type == mkv.VideoTrack {
			hasVideo = true
		}
		if t.Codec == "" {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityError, Message: fmt.Sprintf("track %d: no codec", t.ID)})
		}
		if t.Type == mkv.VideoTrack && (t.Width == nil || t.Height == nil) {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Message: fmt.Sprintf("track %d: video without dimensions", t.ID)})
		}
		if t.Type == mkv.VideoTrack && len(t.CodecPrivate) == 0 {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Message: fmt.Sprintf("track %d: video without CodecPrivate", t.ID)})
		}
		if t.Type == mkv.AudioTrack && t.SampleRate == nil {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Message: fmt.Sprintf("track %d: audio without sample rate", t.ID)})
		}
		if t.Language == "" {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Message: fmt.Sprintf("track %d: no language set", t.ID)})
		}
	}
	if !hasVideo {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Message: "no video track"})
	}

	f, err := fs.DoOpen(path)
	if err != nil {
		return issues, nil
	}
	defer f.Close()

	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityError, Message: fmt.Sprintf("cannot read clusters: %v", err)})
		return issues, nil
	}

	blockCounts := map[uint64]int{}
	var lastTC int64
	var blockTotal int
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
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityError, Message: fmt.Sprintf("cluster read error at block %d: %v", blockTotal, err)})
			break
		}
		blockCounts[blk.TrackNumber]++
		blockTotal++
		if blk.Keyframe {
			hasKeyframe = true
		}
		if blk.Timecode < lastTC-1000 {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Message: fmt.Sprintf("timecode went backwards: %dms → %dms at block %d", lastTC, blk.Timecode, blockTotal)})
		}
		lastTC = blk.Timecode
	}

	if blockTotal == 0 && stat.Size() > 1024 {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Message: "no blocks found (metadata-only file?)"})
	}
	if !hasKeyframe && blockTotal > 0 {
		issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Message: "no keyframes found"})
	}

	for _, t := range c.Tracks {
		if blockCounts[t.ID] == 0 && blockTotal > 0 {
			issues = append(issues, mkv.Issue{Severity: mkv.SeverityWarning, Message: fmt.Sprintf("track %d (%s): no blocks", t.ID, t.Type)})
		}
	}

	return issues, nil
}
