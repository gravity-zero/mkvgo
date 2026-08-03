package ops

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

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
	var titles []string // per-range chapter titles, for the {title} pattern token
	switch {
	case opts.ByChapters:
		if len(c.Chapters) == 0 {
			return nil, fmt.Errorf("no chapters to split by")
		}
		ranges = chaptersToRanges(c.Chapters)
		for _, ch := range c.Chapters {
			titles = append(titles, ch.Title)
		}
	case opts.EveryMs > 0:
		ranges, err = rangesEvery(c.Keyframes, opts.EveryMs)
		if err != nil {
			return nil, err
		}
	default:
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
	used := map[string]bool{}
	for i, r := range ranges {
		if ctx.Err() != nil {
			return outputs, ctx.Err()
		}
		pat := pattern
		if i < len(titles) {
			pat = strings.ReplaceAll(pat, "{title}", sanitizeName(titles[i], i+1))
		}
		name := pat
		if strings.Contains(pat, "%") {
			name = fmt.Sprintf(pat, i+1)
		}
		// A {title}-only pattern with duplicate chapter titles must not make
		// parts overwrite each other: suffix the part number.
		if used[name] {
			ext := filepath.Ext(name)
			name = strings.TrimSuffix(name, ext) + fmt.Sprintf("_%d", i+1) + ext
		}
		used[name] = true
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

// rangesEvery builds keyframe-aligned split ranges of roughly everyMs each:
// every boundary is the first keyframe at/after the previous boundary plus
// everyMs, so each part starts decodable without re-encoding.
func rangesEvery(keyframes []int64, everyMs int64) ([]mkv.TimeRange, error) {
	if len(keyframes) == 0 {
		return nil, fmt.Errorf("no keyframe index (Cues) to segment on; rebuild it with reindex first")
	}
	var bounds []int64
	prev := int64(0)
	for _, k := range keyframes {
		if k >= prev+everyMs {
			bounds = append(bounds, k)
			prev = k
		}
	}
	ranges := make([]mkv.TimeRange, 0, len(bounds)+1)
	start := int64(0)
	for _, b := range bounds {
		ranges = append(ranges, mkv.TimeRange{StartMs: start, EndMs: b})
		start = b
	}
	return append(ranges, mkv.TimeRange{StartMs: start, EndMs: 0}), nil
}

// sanitizeName makes a chapter title safe as a file-name fragment; an empty
// or fully-stripped title falls back to "chapter_<n>".
func sanitizeName(title string, n int) string {
	var b strings.Builder
	for _, r := range title {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == ' ', r == '-', r == '_', r == '.', r == '(', r == ')', r > 127:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), " .")
	if s == "" {
		return fmt.Sprintf("chapter_%02d", n)
	}
	return s
}

func chaptersToRanges(chapters []mkv.Chapter) []mkv.TimeRange {
	ranges := make([]mkv.TimeRange, len(chapters))
	next := nextChapterStarts(chapters)
	for i, ch := range chapters {
		ranges[i] = mkv.TimeRange{StartMs: ch.StartMs, EndMs: ch.EndMs}
		if ranges[i].EndMs == 0 && next[i] > 0 {
			ranges[i].EndMs = next[i]
		}
	}
	return ranges
}

// nextChapterStarts returns, for each chapter, the start of the sibling that
// follows it in TIME, or -1 when nothing does.
//
// Not chapters[i+1]: the reader concatenates every EditionEntry of a file into
// one flat list, each edition restarting at its own first timestamp, so the
// next entry in the list can point back to an earlier moment. Reading the
// following INDEX as the following instant made a chapter end before it began -
// a negative range that Split turned into an empty part, no error - and made
// the last chapter of the first edition vanish from the slice it names.
//
// Strictly greater, so several atoms sharing one timestamp (an edition
// restarting at 0, an atom with no ChapterTimeStart) do not deduce an end of 0
// and fall back to "runs forever", which is the reading this replaced.
//
// Computed for the whole list at once rather than scanned per chapter: Split
// clips the list once per part, so a per-chapter scan made the cut cubic in the
// number of chapters - 4.3 s at 2000 of them, eight times that at every
// doubling.
func nextChapterStarts(chapters []mkv.Chapter) []int64 {
	sorted := make([]int64, len(chapters))
	for i, ch := range chapters {
		sorted[i] = ch.StartMs
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	out := make([]int64, len(chapters))
	for i, ch := range chapters {
		j := sort.Search(len(sorted), func(k int) bool { return sorted[k] > ch.StartMs })
		if j == len(sorted) {
			out[i] = -1
			continue
		}
		out[i] = sorted[j]
	}
	return out
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
	// trim the priming) must be dropped - otherwise one frame of real audio is
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
	// its own timeline - not the source's full list at absolute timestamps.
	// Likewise it lasts as long as its own range, not as long as the source:
	// without the reset every part would declare the whole film's length.
	segMeta := metaForNewDuration(c)
	// The chapters cannot be written yet: a keyframe-aligned part starts on the
	// first keyframe at or after the bound it was cut on, and only the blocks
	// say where that is. Written against the REQUESTED bound they would sit
	// ahead of the picture by however far the GOP reached - the very desync the
	// per-part clipping is there to avoid. So the head books the room and the
	// timestamps are filled in once the part's real start is known.
	chapters := clipChapters(c.Chapters, r.StartMs, r.EndMs)
	segMeta.Chapters = nil
	// A part holds a slice of the source, so the source's content hash and
	// statistics describe something it does not contain. They are measured
	// again while the blocks go by and written after the clusters, in ONE Tags
	// element (the SeekHead points at it, and EditInPlace knows to fold it).
	plan := planContentTags(c.Tags).withStatistics()
	segMeta.Tags = nil // booked below, in the head, filled once measured
	digests, stats := plan.digestsFor(), plan.statsFor()
	if err := mw.WriteMetadata(&segMeta, tracks, durationMs); err != nil {
		return err
	}
	if err := mw.ReserveChapters(chapters); err != nil {
		return err
	}
	if err := mw.ReserveTags(plan.upperBoundTags(tracks)); err != nil {
		return err
	}
	var bounds streamBounds
	trackEnds := make(map[uint64]int64, len(c.Tracks))
	if err := streamToWriter(ctx, mw, c.Path, c.Info.TimecodeScale, fs, streamOpts{
		remap: remap, timeStart: r.StartMs, timeEnd: r.EndMs, keyframeAlign: true,
		videoTracks:    videoTrackSet(c.Tracks),
		contentDigests: digests, contentStats: stats,
		trackEnds: trackEnds,
		bounds:    &bounds,
		progress:  progress,
	}); err != nil {
		return err
	}
	// What the part holds, not what it was asked for: it starts on a keyframe
	// the source chose and keeps the GOP straddling its end, so both bounds move
	// outwards. Declaring the requested range instead left the seek bar of every
	// part describing a slice that is not the one inside it.
	var extent int64
	for _, end := range trackEnds {
		if end > extent {
			extent = end
		}
	}
	if err := mw.RestateDuration(extent); err != nil {
		return err
	}
	// clipChapters rebased on the requested bound; the part actually begins at
	// bounds.startMs, so close the difference. It is never negative - the first
	// kept frame is at or after the bound - and a chapter that ends up before
	// the part's first frame lands on 0, where the picture it names now is.
	if err := mw.WriteReservedChapters(rebaseChapters(chapters, bounds.startMs-r.StartMs)); err != nil {
		return err
	}
	if err := mw.WriteReservedTags(plan.tagsForOutput(tracks, digests, stats)); err != nil {
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

// rebaseChapters moves an already-clipped list back by deltaMs, keeping the
// list itself intact: the same chapters, in the same order, so it still fits
// the slot ReserveChapters booked for it. Timestamps stop at zero, which is
// where a chapter naming a moment before the part's first frame belongs.
func rebaseChapters(chapters []mkv.Chapter, deltaMs int64) []mkv.Chapter {
	if deltaMs == 0 || len(chapters) == 0 {
		return chapters
	}
	out := make([]mkv.Chapter, len(chapters))
	for i, ch := range chapters {
		ch.StartMs = max0(ch.StartMs - deltaMs)
		if ch.EndMs > 0 {
			ch.EndMs = max0(ch.EndMs - deltaMs)
		}
		ch.SubChapters = rebaseChapters(ch.SubChapters, deltaMs)
		out[i] = ch
	}
	return out
}

func max0(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// clipChapters keeps the chapters (recursively) that overlap [startMs, endMs)
// and rebases them onto the segment's own timeline. endMs == 0 means
// "until the end of the source".
func clipChapters(chapters []mkv.Chapter, startMs, endMs int64) []mkv.Chapter {
	var out []mkv.Chapter
	next := nextChapterStarts(chapters)
	for i, ch := range chapters {
		if endMs > 0 && ch.StartMs >= endMs {
			continue
		}
		// ChapterTimeEnd is optional and most muxers leave it out: such a chapter
		// runs until the next one starts (the same reading chaptersToRanges uses
		// to cut on them). Taking a missing end for "no end" instead made every
		// chapter look like it spanned the rest of the file, so each part
		// inherited all the chapters before it, collapsed onto its own start.
		// nextChapterStart says why the following INDEX is not the following
		// instant; -1 is the honest "nothing follows", which does run to the end.
		end := ch.EndMs
		if end == 0 {
			end = next[i]
		}
		if end > 0 && end <= startMs {
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
