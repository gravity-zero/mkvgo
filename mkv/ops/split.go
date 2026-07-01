package ops

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func Split(ctx context.Context, opts mkv.SplitOptions, extra ...mkv.Options) ([]string, error) {
	fs := mkv.FSFrom(extra)
	c, err := reader.OpenWithFS(ctx, opts.SourcePath, fs)
	if err != nil {
		return nil, err
	}

	if err := fs.DoMkdirAll(opts.OutputDir, 0755); err != nil {
		return nil, err
	}

	pattern := opts.Pattern
	if pattern == "" {
		pattern = "part_%03d.mkv"
	}

	var ranges []mkv.TimeRange
	if opts.ByChapters {
		if len(c.Chapters) == 0 {
			return nil, fmt.Errorf("no chapters to split by")
		}
		ranges = chaptersToRanges(c.Chapters)
	} else {
		ranges = opts.Ranges
	}
	if len(ranges) == 0 {
		return nil, fmt.Errorf("no split ranges specified")
	}

	remap := make(map[uint64]uint64, len(c.Tracks))
	for _, t := range c.Tracks {
		remap[t.ID] = t.ID
	}

	var outputs []string
	for i, r := range ranges {
		if ctx.Err() != nil {
			return outputs, ctx.Err()
		}
		name := fmt.Sprintf(pattern, i+1)
		outPath, err := safePath(opts.OutputDir, name)
		if err != nil {
			return outputs, err
		}

		durationMs := r.EndMs - r.StartMs
		if r.EndMs == 0 {
			durationMs = c.DurationMs - r.StartMs
		}

		if err := splitRange(ctx, c, outPath, r, remap, durationMs, fs, mkv.ProgressFrom(extra)); err != nil {
			return outputs, fmt.Errorf("part %d: %w", i+1, err)
		}
		outputs = append(outputs, outPath)
	}
	return outputs, nil
}

func chaptersToRanges(chapters []mkv.Chapter) []mkv.TimeRange {
	ranges := make([]mkv.TimeRange, len(chapters))
	for i, ch := range chapters {
		ranges[i] = mkv.TimeRange{StartMs: ch.StartMs, EndMs: ch.EndMs}
		if ranges[i].EndMs == 0 && i+1 < len(chapters) {
			ranges[i].EndMs = chapters[i+1].StartMs
		}
	}
	return ranges
}

func splitRange(ctx context.Context, c *mkv.Container, outPath string, r mkv.TimeRange, remap map[uint64]uint64, durationMs int64, fs *mkv.FS, progress mkv.ProgressFunc) (err error) {
	out, err := fs.DoCreate(outPath)
	if err != nil {
		return err
	}
	defer closeWithErr(out, &err)

	mw := writer.NewMKVWriter(out)
	if err := mw.WriteStart(); err != nil {
		return err
	}
	// Only the first segment starts with the original encoder priming. A later
	// segment begins on real audio, so its CodecDelay (which a decoder/remux uses to
	// trim the priming) must be dropped — otherwise one frame of real audio is
	// trimmed at the segment start (the -1 AAC frame seen at a split seam).
	tracks := c.Tracks
	if r.StartMs > 0 {
		tracks = make([]mkv.Track, len(c.Tracks))
		copy(tracks, c.Tracks)
		for i := range tracks {
			tracks[i].CodecDelay = 0
		}
	}
	// Each segment gets only the chapters that overlap its range, shifted to
	// its own timeline — not the source's full list at absolute timestamps.
	segMeta := *c
	segMeta.Chapters = clipChapters(c.Chapters, r.StartMs, r.EndMs)
	if err := mw.WriteMetadata(&segMeta, tracks, durationMs); err != nil {
		return err
	}
	if err := streamToWriter(ctx, mw, c.Path, c.Info.TimecodeScale, fs, streamOpts{
		remap: remap, timeStart: r.StartMs, timeEnd: r.EndMs, keyframeAlign: true,
		videoTracks: videoTrackSet(c.Tracks),
		progress:    progress,
	}); err != nil {
		return err
	}
	return mw.Finalize()
}

// videoTrackSet returns the source track numbers of the video tracks, for
// keyframe alignment (audio blocks are all keyframes, so alignment must key on
// video keyframes when a video track exists).
func videoTrackSet(tracks []mkv.Track) map[uint64]bool {
	var set map[uint64]bool
	for _, t := range tracks {
		if t.Type == mkv.VideoTrack {
			if set == nil {
				set = make(map[uint64]bool)
			}
			set[t.ID] = true
		}
	}
	return set
}

// clipChapters keeps the chapters (recursively) that overlap [startMs, endMs)
// and rebases them onto the segment's own timeline. endMs == 0 means
// "until the end of the source".
func clipChapters(chapters []mkv.Chapter, startMs, endMs int64) []mkv.Chapter {
	var out []mkv.Chapter
	for _, ch := range chapters {
		if endMs > 0 && ch.StartMs >= endMs {
			continue
		}
		if ch.EndMs > 0 && ch.EndMs <= startMs {
			continue
		}
		clipped := ch
		clipped.StartMs = ch.StartMs - startMs
		if clipped.StartMs < 0 {
			clipped.StartMs = 0
		}
		if ch.EndMs > 0 {
			end := ch.EndMs
			if endMs > 0 && end > endMs {
				end = endMs
			}
			clipped.EndMs = end - startMs
		}
		clipped.SubChapters = clipChapters(ch.SubChapters, startMs, endMs)
		out = append(out, clipped)
	}
	return out
}
