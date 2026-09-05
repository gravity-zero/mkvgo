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

func ExtractSubtitle(ctx context.Context, srcPath string, trackID uint64, outPath string, opts ...mkv.Options) (err error) {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs, reader.WithoutAttachmentData())
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
	// Filter in the reader, not after the fact: the other tracks' payloads are
	// never delivered, and are seek-skipped when they are larger than the read
	// window. A subtitle track is a handful of blocks among a video track's
	// millions, so the walk stops copying and allocating for all of them.
	br.KeepTracks(trackID)

	out, err := fs.DoCreate(outPath)
	if err != nil {
		return err
	}
	defer closeWithErr(out, &err)

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

// ExtractSubtitleWebVTT extracts the subtitle track trackID from the Matroska
// file at srcPath and writes it as WebVTT to w - the head of the work an
// external subtitle-extraction fork does, in-process. Text codecs are decoded by
// kind: S_TEXT/UTF8 (srt) and S_TEXT/WEBVTT pass through, S_TEXT/ASS is flattened
// to plain text. Each cue's end is its BlockDuration, falling back to the next
// cue's start (then a default) when absent. Bitmap subtitles are not supported.
func ExtractSubtitleWebVTT(ctx context.Context, srcPath string, trackID uint64, w io.Writer, opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs, reader.WithoutAttachmentData())
	if err != nil {
		return err
	}

	var codec string
	found := false
	for _, t := range c.Tracks {
		if t.ID == trackID && t.Type == mkv.SubtitleTrack {
			codec, found = t.Codec, true
			break
		}
	}
	if !found {
		return fmt.Errorf("subtitle track %d not found", trackID)
	}
	if !isTextSubtitle(codec) {
		return fmt.Errorf("subtitle track %d codec %q is not text (cannot convert to WebVTT)", trackID, codec)
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
	br.KeepTracks(trackID) // reader-side filter, as in ExtractSubtitle

	var cues []subtitle.Cue
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
		text := decodeSubtitleCue(codec, blk.Data)
		if text == "" {
			continue
		}
		end := int64(0)
		if blk.Duration > 0 {
			end = blk.Timecode + blk.Duration
		}
		cues = append(cues, subtitle.Cue{StartMs: blk.Timecode, EndMs: end, Text: text})
	}
	subtitle.ResolveCueEnds(cues, defaultSubDurationMs)
	return subtitle.WriteWebVTT(w, cues)
}

// isTextSubtitle reports whether a Matroska subtitle codec short name is a text
// format convertible to WebVTT.
func isTextSubtitle(codec string) bool {
	switch codec {
	case "srt", "ass", "ssa", "webvtt":
		return true
	}
	return false
}

// decodeSubtitleCue turns one subtitle block into WebVTT cue text by codec.
func decodeSubtitleCue(codec string, data []byte) string {
	switch codec {
	case "ass", "ssa":
		return subtitle.FlattenASSBlock(data)
	default: // srt (S_TEXT/UTF8), webvtt (S_TEXT/WEBVTT)
		return trimNulls(data)
	}
}

func MergeSubtitle(ctx context.Context, srcPath, srtPath, dstPath string, lang, name string, opts ...mkv.Options) (err error) {
	entries, err := subtitle.ParseSRT(srtPath)
	if err != nil {
		return fmt.Errorf("parse SRT: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("SRT file is empty")
	}

	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs, reader.WithoutAttachmentData())
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
		// The cue's end time rides as the BlockDuration (BlockGroup); without it
		// the SRT end times are lost and readers fall back to guessed durations.
		var dur int64
		if e.EndMs > e.StartMs {
			dur = e.EndMs - e.StartMs
		}
		subBlocks[i] = mkv.Block{
			TrackNumber: newID,
			Timecode:    e.StartMs,
			Duration:    dur,
			Data:        []byte(e.Text),
		}
	}

	out, err := fs.DoCreate(dstPath)
	if err != nil {
		return err
	}
	defer closeWithErr(out, &err)

	mw := writer.NewMKVWriter(out)
	mw.SetAttachmentSource(attachmentSource(fs))
	if err := mw.WriteStart(); err != nil {
		return err
	}
	// A file with a subtitle track added is not the file it came from: it gets
	// its own derived identity instead of the source's (see derivedSegmentUID).
	// And a cue that outlasts the source is still injected, so the output runs
	// to its end and must say so, exactly as AddTrack does for a longer track -
	// the copied Info.Duration is authoritative, so it has to be cleared.
	meta, durationMs := metaForMergedSubs(c, subBlocks)
	meta.Info.SegmentUID = derivedSegmentUID(&c.Info, srcPath, "merge-subtitle")
	if err := mw.WriteMetadata(&meta, tracks, durationMs); err != nil {
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
