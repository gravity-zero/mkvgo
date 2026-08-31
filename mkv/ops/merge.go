package ops

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// Merge combines the selected tracks of several inputs into one file.
//
// Metadata policy (first-wins): the output's title, chapters, tags and
// attachments come from the FIRST input only; the other inputs contribute
// tracks, not metadata.
func Merge(ctx context.Context, opts mkv.MergeOptions, extra ...mkv.Options) error {
	fs := mkv.FSFrom(extra)
	if len(opts.Inputs) == 0 {
		return fmt.Errorf("no inputs")
	}
	// Honour MergeOptions.Progress unless Options.Progress is already set.
	if opts.Progress != nil && mkv.ProgressFrom(extra) == nil {
		if len(extra) == 0 {
			extra = []mkv.Options{{Progress: opts.Progress}}
		} else {
			o := extra[0] // copy, do not mutate the caller's value
			o.Progress = opts.Progress
			extra = append([]mkv.Options{o}, extra[1:]...)
		}
	}

	var trackInputs []mkv.TrackInput
	for _, inp := range opts.Inputs {
		c, err := reader.OpenWithFS(ctx, inp.SourcePath, fs, reader.WithoutAttachmentData())
		if err != nil {
			return fmt.Errorf("open %s: %w", inp.SourcePath, err)
		}

		want := make(map[uint64]bool)
		if len(inp.TrackIDs) > 0 {
			for _, id := range inp.TrackIDs {
				want[id] = true
			}
		}

		for _, t := range c.Tracks {
			if len(want) > 0 && !want[t.ID] {
				continue
			}
			trackInputs = append(trackInputs, mkv.TrackInput{
				SourcePath: inp.SourcePath,
				TrackID:    t.ID,
				Language:   t.Language,
				Name:       t.Name,
				IsDefault:  t.IsDefault,
			})
		}
	}

	if len(trackInputs) == 0 {
		return fmt.Errorf("no tracks selected")
	}

	first, err := reader.OpenWithFS(ctx, opts.Inputs[0].SourcePath, fs, reader.WithoutAttachmentData())
	if err != nil {
		return err
	}
	muxOpts := mkv.MuxOptions{
		OutputPath:  opts.OutputPath,
		Tracks:      trackInputs,
		Title:       first.Info.Title,
		Chapters:    first.Chapters,
		Tags:        first.Tags,
		Attachments: first.Attachments,
	}

	return Mux(ctx, muxOpts, extra...)
}

func MergeWithSubtitles(ctx context.Context, basePath, srtPath, dstPath string, srtLang, srtName string, extraSources []mkv.MergeInput, opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	if len(extraSources) == 0 {
		return MergeSubtitle(ctx, basePath, srtPath, dstPath, srtLang, srtName, opts...)
	}

	tmpPath := dstPath + ".tmp"
	defer func() { _ = fs.DoRemove(tmpPath) }()

	inputs := append([]mkv.MergeInput{{SourcePath: basePath}}, extraSources...)
	if err := Merge(ctx, mkv.MergeOptions{OutputPath: tmpPath, Inputs: inputs}, opts...); err != nil {
		return err
	}

	return MergeSubtitle(ctx, tmpPath, srtPath, dstPath, srtLang, srtName, opts...)
}
