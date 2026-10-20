package ops

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/internal/splitplan"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func Split(ctx context.Context, opts mkv.SplitOptions, extra ...mkv.Options) ([]string, error) {
	fs := mkv.FSFrom(extra)
	if err := refuseCutSource(opts.SourcePath, fs); err != nil {
		return nil, err
	}
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
		lastKey, kerr := lastVideoKeyMs(ctx, opts.SourcePath, fs, c)
		if kerr != nil {
			return nil, kerr
		}
		var chs []mkv.Chapter
		for _, ch := range splitplan.ByStart(c.Chapters) {
			if lastKey >= 0 && ch.StartMs > lastKey && len(chs) > 0 {
				continue // no keyframe left to open a part on: the previous part runs on through it
			}
			chs = append(chs, ch)
		}
		ranges = chaptersToRanges(chs)
		if len(chs) < len(c.Chapters) {
			latest := 0
			for i, ch := range chs {
				if ch.StartMs > chs[latest].StartMs {
					latest = i
				}
			}
			ranges[latest].EndMs = 0 // the last part runs on through the chapters it could not open
		}
		for _, ch := range chs {
			titles = append(titles, ch.Title)
		}
	case opts.EveryMs > 0:
		ranges, err = rangesEvery(c.Keyframes, opts.EveryMs)
		if err != nil {
			return nil, err
		}
	default:
		ranges = opts.Ranges
		if err := splitplan.CheckRanges(ranges, c.DurationMs); err != nil {
			return nil, err
		}
	}
	if len(ranges) == 0 {
		return nil, fmt.Errorf("no split ranges specified")
	}

	remap := make(map[uint64]uint64, len(c.Tracks))
	for _, t := range c.Tracks {
		remap[t.ID] = t.ID
	}

	uids := splitSegmentUIDs(c, ranges)

	// Every part is named, and checked against the source, before any is written: a part over the source would
	// truncate it before its clusters are read.
	paths := make([]string, len(ranges))
	used := map[string]bool{}
	for i := range ranges {
		title := ""
		if i < len(titles) {
			title = titles[i]
		}
		if paths[i], err = safePath(opts.OutputDir, splitplan.PartName(pattern, i, title, i < len(titles), used)); err != nil {
			return nil, err
		}
		if sameFile(fs, paths[i], opts.SourcePath) {
			return nil, fmt.Errorf("part %d would be written over the source %s: split into another directory, or name the parts with -pattern", i+1, shown.Path(opts.SourcePath))
		}
	}

	var outputs []string
	walk := &sourceWalk{}
	defer walk.Close()
	for i, r := range ranges {
		if ctx.Err() != nil {
			return outputs, ctx.Err()
		}
		outPath := paths[i]

		durationMs := r.EndMs - r.StartMs
		if r.EndMs == 0 {
			durationMs = c.DurationMs - r.StartMs
		}

		if err := splitRange(ctx, c, outPath, r, remap, durationMs, chapterWindowEnd(ranges, i), linkAt(uids, i, &c.Info), fs, walk, mkv.ProgressFrom(extra)); err != nil {
			if nk := (noCutKeyframeError{}); opts.ByChapters && errors.As(err, &nk) && i < len(titles) {
				return outputs, fmt.Errorf("part %d: chapter %q (%d-%d ms) holds no video keyframe: split by time instead (-range or -every, SplitOptions.Ranges or EveryMs), or move the chapter (set-chapters)", i+1, titles[i], r.StartMs, r.EndMs)
			}
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
// package is checked with compare nothing. Each one hashes its part's range too:
// split again at other points, a part names a predecessor that is not the part
// of the first split, which a join must not take for its continuation.
func splitSegmentUIDs(c *mkv.Container, ranges []mkv.TimeRange) [][]byte {
	seed := c.Info.SegmentUID
	if len(seed) == 0 {
		seed = []byte(c.Path)
	}
	uids := make([][]byte, len(ranges))
	for i, r := range ranges {
		h := sha256.New()
		h.Write([]byte("mkvgo.split.segment-uid.v2"))
		h.Write(seed)
		var idx [24]byte
		binary.BigEndian.PutUint64(idx[:8], uint64(i))
		binary.BigEndian.PutUint64(idx[8:16], uint64(r.StartMs))
		binary.BigEndian.PutUint64(idx[16:], uint64(r.EndMs))
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
	if err := mw.ReserveTags(append(plan.upperBoundTags(tracks), splitHoldTags(tracks, nil, 0, true)...)); err != nil {
		return err
	}
	var bounds streamBounds
	trackEnds := make(map[uint64]int64, len(c.Tracks))
	if err := streamToWriter(ctx, mw, c.Path, c.Info.TimecodeScale, fs, streamOpts{
		remap: remap, timeStart: r.StartMs, timeEnd: r.EndMs, keyframeAlign: true,
		videoTracks:    cutVideoTrack(c.Tracks),
		otherVideo:     otherVideoTracks(c.Tracks),
		subtitleTracks: subtitleTrackSet(c.Tracks),
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
	other := otherVideoTracks(c.Tracks)
	for id, end := range trackEnds {
		if end > extent && !other[id] { // another video track runs past the cut to its keyframe, as an MP4 part's edit list hides
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
	if err := mw.WriteReservedTags(append(plan.tagsForOutput(tracks, digests, stats), splitHoldTags(tracks, bounds.heldMs, bounds.startMs, false)...)); err != nil {
		return err
	}
	return mw.Finalize()
}

// splitHoldTags marks each video track's picture held to the cut (held, source timecodes; base, the part's zero);
// widest books room for any of them.
func splitHoldTags(tracks []mkv.Track, held map[uint64]int64, base int64, widest bool) []mkv.Tag {
	var out []mkv.Tag
	for i := range tracks {
		at, ok := held[tracks[i].ID]
		if widest {
			at, ok = math.MinInt64, tracks[i].Type == mkv.VideoTrack
		} else {
			at -= base
		}
		if ok {
			out = append(out, mkv.Tag{TargetID: trackUID(&tracks[i]), SimpleTags: []mkv.SimpleTag{{Name: splitHoldTag, Value: strconv.FormatInt(at, 10)}}})
		}
	}
	return out
}

// splitHoldAt reads, per track number of tracks, the picture its split held to the cut (splitHoldTag).
func splitHoldAt(tracks []mkv.Track, tags []mkv.Tag) map[uint64]int64 {
	var out map[uint64]int64
	for i := range tracks {
		for _, tag := range tags {
			if tag.TargetID != trackUID(&tracks[i]) {
				continue
			}
			for _, st := range tag.SimpleTags {
				if at, err := strconv.ParseInt(st.Value, 10, 64); err == nil && strings.EqualFold(st.Name, splitHoldTag) {
					if out == nil {
						out = map[uint64]int64{}
					}
					out[tracks[i].ID] = at
				}
			}
		}
	}
	return out
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

// cutVideoTrack is the track a split cuts on, as a set: the first video track, as MP4 cuts on (nil without video).
func cutVideoTrack(tracks []mkv.Track) map[uint64]bool {
	for _, t := range tracks {
		if t.Type == mkv.VideoTrack {
			return map[uint64]bool{t.ID: true}
		}
	}
	return nil
}

// otherVideoTracks are the video tracks after the first, the one parts are cut on.
func otherVideoTracks(tracks []mkv.Track) map[uint64]bool {
	cut := cutVideoTrack(tracks)
	var out map[uint64]bool
	for _, t := range tracks {
		if t.Type == mkv.VideoTrack && !cut[t.ID] {
			if out == nil {
				out = map[uint64]bool{}
			}
			out[t.ID] = true
		}
	}
	return out
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

func chaptersToRanges(chapters []mkv.Chapter) []mkv.TimeRange {
	return splitplan.ChapterRanges(chapters)
}

func clipChapters(chapters []mkv.Chapter, startMs, endMs int64) []mkv.Chapter {
	return splitplan.ClipChapters(chapters, startMs, endMs)
}

// refuseCutSource refuses a source shorter than its Segment declares (a download not finished): its parts would stop
// where the file does, the last ones missing or short, with nothing to say so.
func refuseCutSource(path string, fs *mkv.FS) error {
	declared, known, err := segmentDeclaredEndOf(path, fs)
	if err != nil || !known {
		return nil
	}
	st, err := fs.DoStat(path)
	if err != nil || declared <= st.Size() {
		return nil
	}
	return fmt.Errorf("%w: it ends at byte %d of the %d its Segment declares: split it once the file is complete", ErrSourceTruncated, st.Size(), declared)
}

// lastVideoKeyMs is where the last video keyframe presents (-1: no video, or no chapter past the last cued one): the
// Cues prove keyframes up to their last video entry, so only the clusters from there on are read.
func lastVideoKeyMs(ctx context.Context, path string, fs *mkv.FS, c *mkv.Container) (int64, error) {
	video := cutVideoTrack(c.Tracks)
	if len(video) == 0 {
		return -1, nil
	}
	var last *mkv.CuePoint
	for i := range c.Cues {
		if cue := &c.Cues[i]; (cue.Track == 0 || video[cue.Track]) && (last == nil || cue.TimeMs > last.TimeMs) {
			last = cue
		}
	}
	latest := int64(-1)
	for _, ch := range c.Chapters {
		latest = max(latest, ch.StartMs)
	}
	if last != nil && latest <= last.TimeMs {
		return -1, nil
	}
	f, err := fs.DoOpen(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if last != nil {
		return videoKeyFrom(ctx, f, c, video, c.SegmentStart+last.ClusterPos, last.TimeMs)
	}
	// No Cues: the file's tail is read, from a cluster found in its last few MiB, widening the window until it
	// holds a video keyframe; the whole file is read only when one sits at its very start.
	st, err := fs.DoStat(path)
	if err != nil {
		return 0, err
	}
	for window := lastKeyWindow; ; window *= 4 {
		from := max(c.SegmentStart, st.Size()-window)
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return 0, err
		}
		at, err := reader.ResyncToCluster(f, st.Size())
		if err != nil {
			return 0, err
		}
		if at >= 0 {
			if key, err := videoKeyFrom(ctx, f, c, video, at, -1); err != nil || key >= 0 {
				return key, err
			}
		}
		if from == c.SegmentStart {
			return -1, nil
		}
	}
}

// lastKeyWindow is the tail lastVideoKeyMs reads first in a file without Cues: a few GOPs of a high-rate film.
var lastKeyWindow int64 = 32 << 20

// videoKeyFrom is the latest video keyframe from the cluster at offset on (key when there is none), block headers
// only: the payloads are skipped.
func videoKeyFrom(ctx context.Context, f io.ReadSeeker, c *mkv.Container, video map[uint64]bool, offset, key int64) (int64, error) {
	br, err := reader.NewBlockReaderAt(f, c.Info.TimecodeScale, offset)
	if err != nil {
		return 0, err
	}
	br.SetSegmentEnd(c.SegmentEnd)
	br.SetHeaderOnly(true)
	for id := range video {
		br.KeepTracks(id) // the other tracks' blocks are skipped unread
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		blk, err := br.Next()
		if err == io.EOF {
			return key, nil
		}
		if err != nil {
			return 0, err
		}
		if blk.Keyframe && video[blk.TrackNumber] {
			key = max(key, blk.Timecode)
		}
	}
}
