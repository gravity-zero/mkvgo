package ops

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func MergeASS(ctx context.Context, srcPath, assPath, dstPath string, lang, name string, opts ...mkv.Options) error {
	ass, err := subtitle.ParseASS(assPath)
	if err != nil {
		return fmt.Errorf("parse ASS: %w", err)
	}
	if len(ass.Events) == 0 {
		return fmt.Errorf("ASS file has no dialogue events")
	}

	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs)
	if err != nil {
		return err
	}

	newID := uint64(len(c.Tracks) + 1)
	codec := "ass"
	if strings.HasSuffix(strings.ToLower(assPath), ".ssa") {
		codec = "ssa"
	}

	subTrack := mkv.Track{
		ID: newID, Type: mkv.SubtitleTrack, Codec: codec,
		Language: lang, Name: name, CodecPrivate: []byte(ass.Header),
	}
	tracks := append(c.Tracks, subTrack)

	subBlocks := make([]mkv.Block, len(ass.Events))
	for i, ev := range ass.Events {
		subBlocks[i] = mkv.Block{
			TrackNumber: newID, Timecode: ev.StartMs,
			Data: []byte(fmt.Sprintf("%d,0,%s", i, ev.Fields)),
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
		remap: identityRemap(c.Tracks), extraSubs: subBlocks, progress: mkv.ProgressFrom(opts),
	}); err != nil {
		return err
	}
	return mw.Finalize()
}

func ExtractASS(ctx context.Context, srcPath string, trackID uint64, outPath string, opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs)
	if err != nil {
		return err
	}

	var track *mkv.Track
	for i := range c.Tracks {
		if c.Tracks[i].ID == trackID && c.Tracks[i].Type == mkv.SubtitleTrack {
			track = &c.Tracks[i]
			break
		}
	}
	if track == nil {
		return fmt.Errorf("subtitle track %d not found", trackID)
	}
	if track.Codec != "ass" && track.Codec != "ssa" {
		return fmt.Errorf("track %d is %s, not ASS/SSA", trackID, track.Codec)
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

	if len(track.CodecPrivate) > 0 {
		if _, err := fmt.Fprintf(out, "%s\n", track.CodecPrivate); err != nil {
			return fmt.Errorf("write ASS header: %w", err)
		}
	}

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
		parts := strings.SplitN(text, ",", 3)
		if len(parts) < 3 {
			continue
		}
		start := subtitle.FormatASSTimestamp(blk.Timecode)
		end := subtitle.FormatASSTimestamp(blk.Timecode + defaultSubDurationMs)
		if _, err := fmt.Fprintf(out, "Dialogue: %s,%s,%s,%s\n", parts[1], start, end, parts[2]); err != nil {
			return fmt.Errorf("write ASS dialogue: %w", err)
		}
	}
	return nil
}
