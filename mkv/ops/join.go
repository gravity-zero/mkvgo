package ops

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func Join(ctx context.Context, sources []string, dstPath string, opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	if len(sources) == 0 {
		return fmt.Errorf("no sources to join")
	}

	first, err := reader.OpenWithFS(ctx, sources[0], fs)
	if err != nil {
		return err
	}

	for _, src := range sources[1:] {
		c, err := reader.OpenWithFS(ctx, src, fs)
		if err != nil {
			return err
		}
		if len(c.Tracks) != len(first.Tracks) {
			return fmt.Errorf("%s has %d tracks, expected %d", src, len(c.Tracks), len(first.Tracks))
		}
		for i, t := range c.Tracks {
			if t.Type != first.Tracks[i].Type {
				return fmt.Errorf("%s track %d: type %s, expected %s", src, i+1, t.Type, first.Tracks[i].Type)
			}
		}
	}

	out, err := fs.DoCreate(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()

	mw := writer.NewMKVWriter(out)
	if err := mw.WriteStart(); err != nil {
		return err
	}

	var totalDurationMs int64
	for _, src := range sources {
		c, _ := reader.OpenWithFS(ctx, src, fs)
		totalDurationMs += c.DurationMs
	}

	if err := mw.WriteMetadata(first, first.Tracks, totalDurationMs); err != nil {
		return err
	}

	var timeOffset int64
	for _, src := range sources {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c, err := reader.OpenWithFS(ctx, src, fs)
		if err != nil {
			return err
		}

		remap := make(map[uint64]uint64, len(c.Tracks))
		for i, t := range c.Tracks {
			remap[t.ID] = first.Tracks[i].ID
		}

		if err := streamToWriter(ctx, mw, src, c.Info.TimecodeScale, fs, streamOpts{
			remap: remap, timeOffset: timeOffset,
		}); err != nil {
			return fmt.Errorf("join %s: %w", src, err)
		}
		timeOffset += c.DurationMs
	}

	return mw.Finalize()
}
