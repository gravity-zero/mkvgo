package ops

import (
	"context"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

const defaultSubDurationMs = 3000

func ExtractSubtitle(ctx context.Context, srcPath string, trackID uint64, outPath string, opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs)
	if err != nil {
		return err
	}

	var found bool
	for _, t := range c.Tracks {
		if t.ID == trackID && t.Type == mkv.SubtitleTrack {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("subtitle track %d not found", trackID)
	}

	f, err := fs.DoOpen(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		return err
	}

	out, err := fs.DoCreate(outPath)
	if err != nil {
		return err
	}
	defer out.Close()

	seq := 1
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if blk.TrackNumber != trackID {
			continue
		}

		text := trimNulls(blk.Data)
		if len(text) == 0 {
			continue
		}

		endMs := blk.Timecode + defaultSubDurationMs

		if _, err := fmt.Fprintf(out, "%d\n%s --> %s\n%s\n\n",
			seq,
			subtitle.FormatSRTTime(blk.Timecode),
			subtitle.FormatSRTTime(endMs),
			text,
		); err != nil {
			return fmt.Errorf("write subtitle entry: %w", err)
		}
		seq++
	}
	return nil
}

func MergeSubtitle(ctx context.Context, srcPath, srtPath, dstPath string, lang, name string, opts ...mkv.Options) error {
	entries, err := subtitle.ParseSRT(srtPath)
	if err != nil {
		return fmt.Errorf("parse SRT: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("SRT file is empty")
	}

	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs)
	if err != nil {
		return err
	}

	newID := uint64(len(c.Tracks) + 1)
	subTrack := mkv.Track{
		ID:       newID,
		Type:     mkv.SubtitleTrack,
		Codec:    "srt",
		Language: lang,
		Name:     name,
	}
	tracks := append(c.Tracks, subTrack)

	subBlocks := make([]mkv.Block, len(entries))
	for i, e := range entries {
		subBlocks[i] = mkv.Block{
			TrackNumber: newID,
			Timecode:    e.StartMs,
			Data:        []byte(e.Text),
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
	if err := mw.WriteMetadata(c, tracks, c.DurationMs); err != nil {
		return err
	}

	if err := streamToWriter(ctx, mw, srcPath, c.Info.TimecodeScale, fs, streamOpts{
		remap: identityRemap(c.Tracks), extraSubs: subBlocks,
		progress: mkv.ProgressFrom(opts),
	}); err != nil {
		return err
	}
	return mw.Finalize()
}

func trimNulls(data []byte) string {
	for len(data) > 0 && data[len(data)-1] == 0 {
		data = data[:len(data)-1]
	}
	return string(data)
}
