package ops

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/gravity-zero/mkvgo/internal/splitplan"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func Split(ctx context.Context, opts mkv.SplitOptions, extra ...mkv.Options) ([]string, error) {
	fs := mkv.FSFrom(extra)
	c, err := reader.OpenWithFS(ctx, opts.SourcePath, fs, reader.WithoutAttachmentData())
	if err != nil {
		return nil, err
	}

	pattern := opts.Pattern
	if pattern == "" {
		pattern = "part_%03d.mkv"
	}
	if err := splitplan.CheckPattern(pattern); err != nil {
		return nil, err
	}
	if err := fs.DoMkdirAll(opts.OutputDir, 0755); err != nil {
		return nil, err
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

	uids := splitSegmentUIDs(c, len(ranges))

	var outputs []string
	used := map[string]bool{}
	walk := &sourceWalk{}
	defer walk.Close()
	for i, r := range ranges {
		if ctx.Err() != nil {
			return outputs, ctx.Err()
		}
		title := ""
		if i < len(titles) {
			title = titles[i]
		}
		name := splitplan.PartName(pattern, i, title, i < len(titles), used)
		outPath, err := safePath(opts.OutputDir, name)
		if err != nil {
			return outputs, err
		}

		durationMs := r.EndMs - r.StartMs
		if r.EndMs == 0 {
			durationMs = c.DurationMs - r.StartMs
		}

		if err := splitRange(ctx, c, outPath, r, remap, durationMs, chapterWindowEnd(ranges, i), linkAt(uids, i, &c.Info), fs, walk, mkv.ProgressFrom(extra)); err != nil {
			return outputs, fmt.Errorf("part %d: %w", i+1, err)
		}
		outputs = append(outputs, outPath)
	}
	return outputs, nil
}

// segmentLink is one part's place in the chain of parts: its own identity, and
// the identity of the parts on either side of it.
type segmentLink struct {
	uid, prev, next []byte
}

// splitSegmentUIDs derives one SegmentUID per part, and linkAt chains them.
//
// A part is a new segment and owes itself an identity: carrying the source's
// Info over left twelve files all claiming to BE the source, which is what a
// SegmentUID is not allowed to do. Chained through PrevUID/NextUID they say
// more than that - they say these files are consecutive slices of ONE timeline,
// which is exactly what Join needs to know to put the seam back where the cut
// was rather than after everything the previous part happens to hold.
//
// Derived from the source rather than drawn at random: splitting the same file
// twice has to write the same bytes, or the byte-for-byte comparisons this
// package is checked with compare nothing.
func splitSegmentUIDs(c *mkv.Container, n int) [][]byte {
	seed := c.Info.SegmentUID
	if len(seed) == 0 {
		seed = []byte(c.Path)
	}
	uids := make([][]byte, n)
	for i := range uids {
		h := sha256.New()
		h.Write([]byte("mkvgo.split.segment-uid.v1"))
		h.Write(seed)
		var idx [8]byte
		binary.BigEndian.PutUint64(idx[:], uint64(i))
		h.Write(idx[:])
		uids[i] = h.Sum(nil)[:16] // a SegmentUID is 128 bits
	}
	return uids
}

// linkAt chains part i to its neighbours. The ends of the chain keep the
// SOURCE's own links: a source that is itself a slice of a larger timeline has
// a predecessor, and the first part still succeeds it - dropping that link
// threw away a statement that stayed true. Same for the last part and the
// source's successor.
func linkAt(uids [][]byte, i int, src *mkv.SegmentInfo) segmentLink {
	l := segmentLink{uid: uids[i], prev: src.PrevUID, next: src.NextUID}
	if i > 0 {
		l.prev = uids[i-1]
	}
	if i+1 < len(uids) {
		l.next = uids[i+1]
	}
	return l
}

// chapterWindowEnd is how far a part's chapter window may reach: the head has
// to book room before the blocks say where the cut fell, so a bound is needed
// that holds in both passes.
//
// The part stops on the first keyframe at or after its requested end, and that
// keyframe is inside the NEXT range whenever the two are contiguous - a range
// with no keyframe of its own is an explicit error, so a range that survives
// opens on a keyframe before its own end. Ranges with a gap between them say
// nothing of the sort: for those the window is the rest of the file, which is
// always safe and costs a few hundred bytes of Void per chapter (`-range`
// splits are counted on one hand, while `-chapters` and `-every`, the ones that
// produce hundreds of parts, are contiguous by construction).
//
// The SAME value bounds the booking and the selection, so the two can never
// disagree: where the cut does reach past it - a split that is about to fail on
// the next range - the part simply keeps the chapters it was asked for, and the
// failure is reported by the range that has no keyframe rather than by a slot
// that came up short.
func chapterWindowEnd(ranges []mkv.TimeRange, i int) int64 {
	if i+1 >= len(ranges) || ranges[i].EndMs != ranges[i+1].StartMs {
		return 0 // to the end of the source
	}
	return ranges[i+1].EndMs
}

func splitRange(ctx context.Context, c *mkv.Container, outPath string, r mkv.TimeRange, remap map[uint64]uint64, durationMs, windowEndMs int64, link segmentLink, fs *mkv.FS, walk *sourceWalk, progress mkv.ProgressFunc) (err error) {
	out, err := fs.DoCreate(outPath)
	if err != nil {
		return err
	}
	defer removeUnfinished(fs, outPath, &err) // a failed write leaves no truncated output
	defer closeWithErr(out, &err)

	mw := writer.NewMKVWriter(out)
	mw.SetAttachmentSource(attachmentSource(fs))
	if err := writeStartLike(mw, c, c.Tracks); err != nil {
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
	segMeta.Info.SegmentUID, segMeta.Info.PrevUID, segMeta.Info.NextUID = link.uid, link.prev, link.next
	// The chapters cannot be written yet, and not only because their timestamps
	// wait on the part's real start: WHICH of them belong here waits on it too.
	// A keyframe-aligned part opens on the first keyframe at or after the bound
	// it was cut on and keeps the GOP straddling its end, so a marker sitting
	// between the requested bound and that keyframe names a frame the PREVIOUS
	// part holds. Selected on the requested bound it came here anyway, with
	// nowhere to sit but zero - a chapter announced up to a GOP early, at the
	// wrong picture, on every part of a split.
	//
	// So the head books room for every chapter that could possibly land here
	// (the slot is sized for the widest timestamp EBML can encode, so only the
	// LIST has to be bounded, never the values), and the real selection is made
	// against the cut once the blocks have said where it fell.
	segMeta.Chapters, segMeta.OtherEditions = nil, nil // the other editions are not clipped to the part
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
	if err := mw.ReserveChapters(clipChapters(c.Chapters, r.StartMs, windowEndMs)); err != nil {
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
		walk:      walk,
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
	// Selected on the cut, which is exact and which consecutive parts share, so
	// every marker goes to the one part that holds its frame. clipChapters then
	// counts from that cut, while the part's own zero is bounds.startMs - the
	// audio of the same instant, which may sit slightly below it - so close that
	// difference. It is never positive: the first frame is the cut.
	chapters := clipChapters(c.Chapters, bounds.cutStartMs, clampWindow(bounds.cutEndMs, windowEndMs))
	if err := mw.WriteReservedChapters(rebaseChapters(chapters, bounds.startMs-bounds.cutStartMs)); err != nil {
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

// rebaseChapters moves an already-clipped list by deltaMs (back for a positive
// delta, forward for a negative one), keeping the list itself intact: the same
// chapters, in the same order, so it still fits the slot ReserveChapters booked
// for it. Timestamps stop at zero, which is where a chapter naming a moment
// before the part's first frame belongs.
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

// clampWindow keeps a part's chapter window inside the bound the head booked
// room for; 0 on either side means "no bound".
func clampWindow(endMs, windowEndMs int64) int64 {
	if windowEndMs > 0 && (endMs == 0 || endMs > windowEndMs) {
		return windowEndMs
	}
	return endMs
}

func max0(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// rangesEvery builds keyframe-aligned split ranges of roughly everyMs each.
func rangesEvery(keyframes []int64, everyMs int64) ([]mkv.TimeRange, error) {
	if len(keyframes) == 0 {
		return nil, fmt.Errorf("no keyframe index (Cues) to segment on; rebuild it with reindex first")
	}
	return splitplan.EveryRanges(keyframes, everyMs)
}

func deviceName(s string) bool { return splitplan.DeviceName(s) }

func chaptersToRanges(chapters []mkv.Chapter) []mkv.TimeRange {
	return splitplan.ChapterRanges(chapters)
}

func clipChapters(chapters []mkv.Chapter, startMs, endMs int64) []mkv.Chapter {
	return splitplan.ClipChapters(chapters, startMs, endMs)
}
