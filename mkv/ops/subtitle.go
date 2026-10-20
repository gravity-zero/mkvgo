package ops

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

const defaultSubDurationMs = 3000

// openCues holds the cues an extraction has met but not written, in the order met: each that states no end is ended
// by the first later cue that starts after it (subtitle.SettleCueEnds' rule), and each is written by its own write once
// it and every cue before it have their end.
type openCues struct {
	cues       []openCue
	base       int   // the sequence number of cues[0]
	open       []int // the cues still without an end, by sequence number, starts non-increasing: the latest on top
	mediaEndMs int64
}

type openCue struct {
	start, end int64 // end 0 until known
	text       string
	write      func(endMs int64) error
}

// add queues a cue (endMs 0 when it states none), ending the queued ones it starts after, then writes those now ended.
func (q *openCues) add(startMs, endMs int64, text string, write func(endMs int64) error) error {
	for len(q.open) > 0 {
		c := &q.cues[q.open[len(q.open)-1]-q.base]
		if c.start >= startMs {
			break
		}
		c.end = subtitle.CueEndMs(c.start, c.text, startMs, true, q.mediaEndMs)
		q.open = q.open[:len(q.open)-1]
	}
	if endMs <= startMs {
		endMs = 0
		q.open = append(q.open, q.base+len(q.cues))
	}
	q.cues = append(q.cues, openCue{start: startMs, end: endMs, text: text, write: write})
	n := 0
	for n < len(q.cues) && q.cues[n].end > q.cues[n].start {
		if err := q.cues[n].write(q.cues[n].end); err != nil {
			return err
		}
		n++
	}
	q.cues, q.base = q.cues[n:], q.base+n
	return nil
}

// flush writes the queued cues, each still without an end ended by nextMs (when hasNext), its reading or the media's end.
func (q *openCues) flush(nextMs int64, hasNext bool) error {
	for _, c := range q.cues {
		end := c.end
		if end <= c.start {
			end = subtitle.CueEndMs(c.start, c.text, nextMs, hasNext, q.mediaEndMs)
		}
		if err := c.write(end); err != nil {
			return err
		}
	}
	q.base += len(q.cues)
	q.cues, q.open = q.cues[:0], q.open[:0]
	return nil
}

func ExtractSubtitle(ctx context.Context, srcPath string, trackID uint64, outPath string, opts ...mkv.Options) (err error) {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs, reader.WithoutAttachmentData())
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
	if !isTextSubtitle(track.Codec) {
		return fmt.Errorf("subtitle track %d codec %q is not text (cannot write it as SRT): extract a bitmap track with -format pgs", trackID, track.Codec)
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
	dmg := tolerateSubtitleDamage(br, opts)

	out, err := createOutput(fs, outPath, srcPath)
	if err != nil {
		return err
	}
	defer removeUnfinished(fs, outPath, &err) // a failed write leaves no truncated output
	defer closeWithErr(out, &err)

	seq := 1
	q := openCues{mediaEndMs: c.DurationMs}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		blk, err := br.Next()
		if err == io.EOF {
			if fromMs, ok := dmg.skipped(); ok {
				return q.flush(fromMs, true)
			}
			return q.flush(0, false)
		}
		if err != nil {
			return err
		}
		if fromMs, ok := dmg.skipped(); ok {
			if err := q.flush(fromMs, true); err != nil {
				return err
			}
		}
		payload, err := decompressSubtitleBlock(track, blk.Data)
		if err != nil {
			return err
		}
		text := trimNulls(payload)
		if len(text) == 0 {
			continue
		}
		endMs := int64(0)
		if blk.Duration > 0 {
			endMs = blk.Timecode + blk.Duration
		}
		start := blk.Timecode
		if err := q.add(start, endMs, text, func(endMs int64) error {
			if _, err := fmt.Fprintf(out, "%d\n%s --> %s\n%s\n\n", seq, subtitle.FormatSRTTime(start), subtitle.FormatSRTTime(endMs), text); err != nil {
				return fmt.Errorf("write subtitle entry: %w", err)
			}
			seq++
			return nil
		}); err != nil {
			return err
		}
	}
}

// ExtractSubtitleWebVTT extracts the subtitle track trackID from the Matroska
// file at srcPath and writes it as WebVTT to w - the head of the work an
// external subtitle-extraction fork does, in-process. Text codecs are decoded by
// kind: S_TEXT/UTF8 (srt) and S_TEXT/WEBVTT pass through, S_TEXT/ASS is flattened
// to plain text. Each cue's end is its BlockDuration; absent, subtitle.SettleCueEnds
// ends it once read, at the next cue or at the film's end. Bitmap subtitles are not supported.
//
// When the source's Cues name every block of the track - their count matches a
// trusted NUMBER_OF_FRAMES statistic - the blocks are read where the Cues put
// them and nothing else of the file is; otherwise, or when an indexed block
// does not hold what the Cues say or lies in a damaged region, the file is
// walked. Both give the same WebVTT, and damage is handled the walk's way: a
// region within Options.DamageBytes is passed over (OnSkip), a larger one, or
// one the bound refuses, fails the extraction with nothing written.
func ExtractSubtitleWebVTT(ctx context.Context, srcPath string, trackID uint64, w io.Writer, opts ...mkv.Options) error {
	if ix := verifiedCueIndex(ctx, srcPath, trackID, opts); ix != nil {
		var out bytes.Buffer // written only once every indexed block read as the Cues say
		err := ExtractSubtitleWebVTTFrom(ctx, srcPath, trackID, ix, &out, opts...)
		if err == nil {
			_, err = w.Write(out.Bytes())
			return err
		}
		if !errors.Is(err, ErrIndexStale) {
			return err
		}
	}
	return extractSubtitleWebVTTWalk(ctx, srcPath, trackID, w, opts...)
}

// verifiedCueIndex returns the resolved index the source's Cues give trackID when they are verified complete and
// every entry resolved outside damage, nil when the walk must serve.
func verifiedCueIndex(ctx context.Context, srcPath string, trackID uint64, opts []mkv.Options) *SubtitleIndex {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenMetaWithFS(ctx, srcPath, fs, reader.WithCues(), reader.WithTags())
	if err != nil {
		return nil
	}
	st, err := fs.DoStat(srcPath)
	if err != nil {
		return nil
	}
	ix, verified, err := SubtitleIndexFromCues(c, st.Size(), []uint64{trackID})
	if err != nil || !verified[trackID] {
		return nil
	}
	cued := len(ix.entries[trackID])
	if err := ix.Resolve(ctx, srcPath, opts...); err != nil || len(ix.entries[trackID]) != cued {
		return nil // an entry lost to damage: the walk decides what the region costs
	}
	return ix
}

// extractSubtitleWebVTTWalk is ExtractSubtitleWebVTT walking the whole file.
func extractSubtitleWebVTTWalk(ctx context.Context, srcPath string, trackID uint64, w io.Writer, opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs, reader.WithoutAttachmentData())
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
	codec := track.Codec
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
	dmg := tolerateSubtitleDamage(br, opts)

	var cues []subtitle.Cue
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		blk, err := br.Next()
		if err == io.EOF {
			fromMs, ok := dmg.skipped()
			closeOpenCue(cues, fromMs, ok)
			break
		}
		if err != nil {
			return err
		}
		fromMs, ok := dmg.skipped()
		closeOpenCue(cues, fromMs, ok)
		payload, err := decompressSubtitleBlock(track, blk.Data)
		if err != nil {
			return err
		}
		text := decodeSubtitleCue(codec, payload)
		if text == "" {
			continue
		}
		end := int64(0)
		if blk.Duration > 0 {
			end = blk.Timecode + blk.Duration
		}
		cues = append(cues, subtitle.Cue{StartMs: blk.Timecode, EndMs: end, Text: text})
	}
	subtitle.SettleCueEnds(cues, c.DurationMs)
	if err := subtitle.WriteWebVTT(w, cues); err != nil {
		return err
	}
	return truncatedSource(srcPath, fs, cues)
}

// ErrSourceTruncated reports a source that stops before the end its Segment declares (a download not finished):
// a WebVTT extraction wrote what it holds, Split wrote nothing.
var ErrSourceTruncated = errors.New("the source stops before its declared end")

// truncatedSource returns ErrSourceTruncated, saying how far the written cues reach, when the file is shorter
// than its Segment declares.
func truncatedSource(path string, fs *mkv.FS, cues []subtitle.Cue) error {
	declared, known, err := segmentDeclaredEndOf(path, fs)
	if err != nil || !known {
		return nil
	}
	st, err := fs.DoStat(path)
	if err != nil || declared <= st.Size() {
		return nil
	}
	var reach int64
	if n := len(cues); n > 0 {
		reach = cues[n-1].EndMs
	}
	return fmt.Errorf("%w: it ends at byte %d of the %d its Segment declares, so the WebVTT holds the cues up to %d ms: extract again once the file is complete", ErrSourceTruncated, st.Size(), declared, reach)
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

	subTrack := appendedTrack(c.Tracks, mkv.Track{
		Type:     mkv.SubtitleTrack,
		Codec:    "srt",
		Language: lang,
		Name:     name,
	})
	newID := subTrack.ID
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
			Keyframe:    true, // a text cue depends on no other
			Data:        []byte(e.Text),
		}
	}
	// Blocks go out in time order: a cue listed out of order would land in a cluster it is timed before.
	slices.SortStableFunc(subBlocks, func(a, b mkv.Block) int { return cmp.Compare(a.Timecode, b.Timecode) })

	out, err := createOutput(fs, dstPath, srcPath, srtPath)
	if err != nil {
		return err
	}
	defer removeUnfinished(fs, dstPath, &err) // a failed write leaves no truncated output
	defer closeWithErr(out, &err)

	mw := writer.NewMKVWriter(out)
	mw.SetAttachmentSource(attachmentSource(fs))
	if err := writeStartLike(mw, c, tracks, subTrack); err != nil {
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
