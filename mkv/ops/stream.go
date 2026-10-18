package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"math"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

const defaultClusterDurationMs = 1000

// maxBlockRelTC is the widest offset a SimpleBlock can carry from its cluster's
// timestamp: the field is a signed 16-bit count of TIMECODE UNITS, not of
// milliseconds.
const maxBlockRelTC = 32767

// clusterSpanMs is how long a rewritten cluster may run, in milliseconds, on a
// file with this timebase: the 1s default, unless the timebase is fine enough
// that a second of media no longer fits a block's 16-bit offset.
//
// At the 1 ms default a cluster could run 32 seconds, so this changes nothing
// for all but a handful of files. On a timebase of ~1/48000 s - 48 units to the
// millisecond - a second is 48003 units and every rewriting op (RemoveTrack,
// Split, Join, Mux, the WebM remux) refused the file outright rather than
// closing the cluster sooner.
func clusterSpanMs(scale int64) int64 {
	if scale <= 0 {
		scale = mkv.DefaultTimecodeScale
	}
	span := maxBlockRelTC * scale / mkv.DefaultTimecodeScale
	if span >= defaultClusterDurationMs {
		return defaultClusterDurationMs
	}
	// Zero is a real answer, not a floor to round away from: on a timebase so
	// fine that not even a millisecond fits 16 bits, every block has to open its
	// own cluster, and each one then carries an offset of zero. A one-millisecond
	// floor here looks safer and is not - it hands back a span that still
	// overflows, which is the bug this function exists to prevent.
	return span
}

type streamOpts struct {
	remap map[uint64]uint64
	// outScale is the TimecodeScale the OUTPUT declares, when it differs from
	// the source's. The one scale served both jobs before - decoding the
	// source's ticks into milliseconds AND encoding them back on the way out -
	// so appending a file muxed at a different scale wrote ticks against a
	// divisor the header does not declare: a source at 0.1 ms joined into a
	// 1 ms file came out stretched tenfold, silently. 0 means "same as the
	// source", which is every caller but Join. streamMergeToWriter has always
	// kept the two apart; this is that distinction, brought here.
	outScale int64
	// timeOffset shifts every block of this source by the same amount, which is
	// what keeps the tracks aligned with each other when Join appends a file.
	timeOffset int64
	// trackEnds, when non-nil, is filled with each output track's next free start
	// timecode (its last frame's end). Join takes the largest as the next file's
	// timeOffset: the end of the whole file, measured rather than declared.
	trackEnds map[uint64]int64
	// contentDigests and contentStats, when non-nil, accumulate each OUTPUT
	// track's payload digest and media statistics as the blocks go by, keyed by
	// output track number and filled lazily. An op that cuts or concatenates
	// uses them to write tags describing what it ACTUALLY wrote: copying the
	// source's CONTENT_SHA256 and statistics over instead makes the output
	// carry a checksum of somebody else's content. Both survive across calls,
	// so Join can accumulate over all its sources.
	contentDigests map[uint64]hash.Hash
	contentStats   map[uint64]*trackStats
	timeStart      int64
	timeEnd        int64
	keyframeAlign  bool // split on keyframe boundaries
	// videoTracks holds the SOURCE track numbers of video tracks. keyframeAlign
	// aligns on these: every audio block is flagged keyframe, so aligning on
	// "any keyframe" would start a segment mid-GOP (corrupt video until the
	// next real video keyframe). Empty means no video track: any keyframe cuts.
	videoTracks map[uint64]bool
	// subtitleTracks holds the SOURCE track numbers of subtitle tracks: a cue on screen across a cut opens the
	// next part too (walk.onScreen), and lastSubs / dropLeadingSub let Join keep it once.
	subtitleTracks map[uint64]bool
	// lastSubs, when non-nil, receives each output subtitle track's last cue (source timeline).
	lastSubs map[uint64]mkv.Block
	// dropLeadingSub holds, per output track, a cue the previous linked part already shows past its end: the
	// copy a split opened this part on is dropped.
	dropLeadingSub map[uint64][]byte
	extraSubs      []mkv.Block
	// bounds, when non-nil, receives where the segment really began. A
	// keyframe-aligned cut starts where the source's GOPs fall, not where it was
	// asked to, so a caller that has to agree with the blocks - chapters to
	// rebase against the same instant - has to be told.
	bounds   *streamBounds
	progress mkv.ProgressFunc
	// walk, when non-nil, carries the source walk across consecutive calls (Split's parts).
	walk *sourceWalk
}

// sourceWalk carries one source walk across consecutive keyframe-aligned calls:
// a call whose range starts at or after the previous call's end opens on the
// block that call stopped at, the first cut keyframe at or after its end, which
// is where a walk from the top would open the gate too.
type sourceWalk struct {
	f      mkv.ReadSeekCloser
	br     *reader.BlockReader
	carry  *mkv.Block // the block the previous call stopped on, if it stopped before EOF
	stopMs int64      // the end that call was cut on
	maxMs  int64      // the latest timecode read before carry: every such block is no cut keyframe
	// onScreen holds the subtitle cues the previous call wrote that are still on screen at its cut: the next
	// call, opening on carry, shows them from its own start for the time they have left.
	onScreen []mkv.Block
}

// walkReader is a BlockReader that may outlive the call that opened it.
type walkReader struct {
	*reader.BlockReader
	f mkv.ReadSeekCloser
}

func (r walkReader) Close() error { return r.f.Close() }

// reader returns the walk for a range starting at startMs, and the block to
// deliver before the reader's next one.
func (w *sourceWalk) reader(fs *mkv.FS, srcPath string, scale, startMs int64, progress mkv.ProgressFunc) (walkReader, *mkv.Block, error) {
	if w != nil && w.carry != nil && startMs >= w.stopMs {
		first := w.carry
		w.carry = nil
		return walkReader{w.br, w.f}, first, nil
	}
	f, err := fs.DoOpen(srcPath)
	if err != nil {
		return walkReader{}, nil, err
	}
	br, err := reader.NewBlockReader(f, scale)
	if err != nil {
		f.Close()
		return walkReader{}, nil, err
	}
	if progress != nil {
		if stat, _ := fs.DoStat(srcPath); stat != nil {
			br.SetProgress(progress, stat.Size())
		}
	}
	if w != nil {
		w.Close()
		w.f, w.br = f, br
	}
	return walkReader{br, f}, nil, nil
}

// stopped records the block a call cut its range on, for the next call to open on.
func (w *sourceWalk) stopped(blk mkv.Block, endMs, maxMs int64) {
	if w != nil {
		w.carry, w.stopMs, w.maxMs = &blk, endMs, maxMs
	}
}

// Close releases the source.
func (w *sourceWalk) Close() error {
	if w == nil || w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f, w.br, w.carry, w.onScreen = nil, nil, nil, nil
	return err
}

// streamBounds reports where a segment's own timeline starts in its source, and
// which slice of that source it claims.
type streamBounds struct {
	// startMs is the source timecode the output's zero stands for: the
	// requested bound normally, the first cut keyframe at or after it when the
	// cut is keyframe-aligned.
	startMs int64
	// cutStartMs and cutEndMs are the keyframes the segment opened and stopped
	// on - the cut itself, which is exact, as opposed to startMs, which the
	// audio of that same instant may sit a little below. cutEndMs is 0 when the
	// segment ran to the end of the source.
	//
	// Consecutive segments SHARE a cut: one stops on the keyframe the next one
	// opens on. Anything that has to be handed to exactly one of them - a
	// chapter marker, which belongs where its frame is - is decided on these
	// and on nothing else. The requested bounds do not have that property: the
	// segment before the cut keeps the GOP straddling it, so a marker between
	// the requested bound and the keyframe names a frame that is in the
	// PREVIOUS segment, and giving it to this one puts it on the wrong picture.
	cutStartMs int64
	cutEndMs   int64
}

// trackEndState tracks one output track's end while streaming: the last frame's
// timecode, the smallest positive inter-frame gap (a frame-duration estimate when
// blocks carry no explicit duration) and the largest explicit block duration.
type trackEndState struct {
	maxTC   int64
	prevTC  int64
	hasPrev bool
	minGap  int64
	// lastDur is the explicit duration of the block at maxTC - the only one that
	// says where the track ends. Keeping the LARGEST duration of the whole track
	// instead let a long sign early on decide: a 12 s cue at 0:01 in a 10 s part
	// put the end at 21 s, and Join opened 11 s of dead air at the seam.
	lastDur int64
}

func streamToWriter(ctx context.Context, mw *writer.MKVWriter, srcPath string, timecodeScale int64, fs *mkv.FS, opts streamOpts) error {
	br, carry, err := opts.walk.reader(fs, srcPath, timecodeScale, opts.timeStart, opts.progress)
	if err != nil {
		return err
	}
	if opts.walk == nil {
		defer br.Close()
	}

	var cluster []mkv.Block
	clusterTS := int64(-1)
	subIdx := 0
	gateSkipped := false // keyframeAlign dropped in-range blocks waiting for a cut keyframe
	maxMs := int64(math.MinInt64)
	if carry != nil {
		// A walk from the top would have dropped the blocks read before carry at the gate.
		maxMs = opts.walk.maxMs
		gateSkipped = opts.timeStart > 0 && maxMs >= opts.timeStart
	}
	endStates := map[uint64]*trackEndState{}
	var carried, queue, shown []mkv.Block // cues to open on, cues to write next, cues written (source timeline)
	var opening []mkv.Block               // the carried cues as the source shows them, before they open at the cut
	if carry != nil {
		carried, opts.walk.onScreen = opts.walk.onScreen, nil
	}
	firstTC, firstSet := int64(0), false

	// The output's timebase decides how long a cluster may run: block offsets
	// are 16-bit counts of ITS units, not of milliseconds.
	outScale := opts.outScale
	if outScale <= 0 {
		outScale = timecodeScale
	}
	clusterMs := clusterSpanMs(outScale)

	// base is the source timecode that lands on opts.timeOffset in the output:
	// blocks are shifted by base-timeOffset on their way out, and they are held
	// at their SOURCE timecode until then.
	//
	// Without keyframe alignment the base is the requested bound, known before
	// a single block is read. With it, the segment really starts on the first
	// cut keyframe at or after that bound, wherever the source's GOP puts it -
	// measured on real films, 2.4 s to 5.5 s later. Rebasing on the REQUESTED
	// bound opened every part with a hole of exactly that length: played alone
	// it froze on its first frame, and joined back it pushed the whole seam out
	// by the same amount.
	//
	// It cannot simply be the first block accepted either. The gate opens on a
	// video keyframe, and the audio of that same instant may still follow it in
	// file order while sitting slightly BEHIND it in time - 182 ms on a real
	// remux. Those blocks would rebase to a negative timestamp. So the decision
	// waits for the first flush and takes the smallest timecode of that cluster,
	// which spans a second of content; anything reordered further than that is
	// refused rather than written out of range.
	base := opts.timeStart
	baseSettled := !opts.keyframeAlign || opts.timeStart <= 0

	// cutStart is the keyframe the segment opened on. Without the gate the
	// segment opens on the bound it was asked for, which is then the cut.
	cutStart, cutStartSet := opts.timeStart, !opts.keyframeAlign || opts.timeStart <= 0

	// shift converts a source timecode to an output one. Only meaningful once
	// the base is settled.
	shift := func() int64 { return base - opts.timeOffset }

	flush := func() error {
		if len(cluster) == 0 {
			return nil
		}
		if !baseSettled {
			base = math.MaxInt64
			for i := range cluster {
				if t := cluster[i].Timecode; t < base && t >= opts.timeStart { // a leading block stays ahead of the zero
					base = t
				}
			}
			baseSettled = true
			for i := range cluster { // a carried cue was on screen at the zero too, not only at the cut
				for _, o := range opening {
					if b := &cluster[i]; b.Timecode == cutStart && base < cutStart && bytes.Equal(b.Data, o.Data) {
						from := max(o.Timecode, base)
						b.Duration += b.Timecode - from
						b.Timecode = from
					}
				}
			}
		}
		s := shift()
		for i := range cluster {
			if cluster[i].Timecode < base && cluster[i].Timecode >= opts.timeStart {
				return fmt.Errorf("block at %dms precedes the segment's first frame (%dms) by more than one cluster: refusing to write a negative timestamp", cluster[i].Timecode, base)
			}
			cluster[i].Timecode -= s
		}
		err := mw.WriteClusterWithCues(clusterTS-s, outScale, cluster)
		cluster = cluster[:0]
		return err
	}

	// injectSubs appends the pending extra subtitle blocks up to the given
	// timecode, rolling the cluster forward when a cue is too far from the
	// cluster start (a SimpleBlock offset must fit in int16 timecode units, so
	// sparse cues cannot all share one cluster).
	injectSubs := func(upTo int64) error {
		for subIdx < len(opts.extraSubs) && opts.extraSubs[subIdx].Timecode <= upTo {
			sub := opts.extraSubs[subIdx]
			if clusterTS >= 0 && sub.Timecode-clusterTS >= clusterMs {
				if err := flush(); err != nil {
					return err
				}
				clusterTS = sub.Timecode
			}
			if clusterTS < 0 {
				clusterTS = sub.Timecode
			}
			cluster = append(cluster, sub)
			subIdx++
		}
		return nil
	}

	// recordEnds publishes each output track's next free start (its last frame's
	// end) so a concatenating caller can rebase the following file per track.
	// Called after the last flush: the ends are in the OUTPUT timeline, and the
	// base that defines it is only settled once a cluster has been written.
	cutEnd := int64(0)

	recordEnds := func() {
		if opts.bounds != nil {
			opts.bounds.startMs = base
			opts.bounds.cutStartMs = cutStart
			opts.bounds.cutEndMs = cutEnd
		}
		if opts.trackEnds == nil {
			return
		}
		out := shift()
		for id, s := range endStates {
			// An explicit Block.Duration is ground truth and wins; the smallest
			// inter-frame gap is only a stand-in for the tracks that carry no
			// duration (video, most audio). Taking the LARGER of the two - as
			// this did - let a sparse track dictate the end: subtitle cues
			// minutes apart made "one frame" mean minutes, pushing the file's
			// end (and, for Join, the next file's whole timeline) out with it.
			frame := s.lastDur
			if frame <= 0 {
				frame = s.minGap
			}
			if frame <= 0 {
				frame = 1 // a single frame seen: nudge to avoid an exact overlap
			}
			opts.trackEnds[id] = s.maxTC + frame - out
		}
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var blk mkv.Block
		prevMax := maxMs
		if len(queue) > 0 {
			blk, queue, err = queue[0], queue[1:], nil
		} else if carry != nil {
			blk, carry = *carry, nil
		} else if blk, err = br.Next(); err == nil && blk.Timecode > maxMs {
			maxMs = blk.Timecode
		}
		if err == io.EOF {
			if clusterTS < 0 && gateSkipped {
				return noCutKeyframeErr(opts.timeStart)
			}
			if err := injectSubs(1 << 62); err != nil {
				return err
			}
			if err := flush(); err != nil {
				return err
			}
			recordEnds()
			return nil
		}
		if err != nil {
			return err
		}

		// cutKeyframe: a keyframe a segment may start or end on. With a video
		// track only a video keyframe qualifies (see videoTracks above).
		cutKeyframe := blk.Keyframe && (len(opts.videoTracks) == 0 || opts.videoTracks[blk.TrackNumber])

		// Stored after the cut keyframe yet timed before it (an open GOP's leading pictures, the sound muxed with
		// them): the part before stopped at the keyframe, so this one keeps them ahead of its zero.
		if opts.timeStart > 0 && blk.Timecode < opts.timeStart && (!opts.keyframeAlign || clusterTS < 0 || opts.timeStart-blk.Timecode > clusterMs) {
			continue
		}
		// keyframeAlign: drop everything (audio included) until the first cut
		// keyframe, so the segment starts decodable and A/V start together.
		if opts.keyframeAlign && opts.timeStart > 0 && clusterTS < 0 && !cutKeyframe {
			gateSkipped = true
			continue
		}
		if opts.timeEnd > 0 && blk.Timecode >= opts.timeEnd {
			if clusterTS < 0 && gateSkipped {
				return noCutKeyframeErr(opts.timeStart)
			}
			if !opts.keyframeAlign || cutKeyframe {
				cutEnd = blk.Timecode
				opts.walk.stopped(blk, opts.timeEnd, prevMax)
				if opts.walk != nil {
					opts.walk.onScreen = cuesOnScreenAt(shown, blk.Timecode)
				}
				break
			}
			// keyframeAlign: the cut lands on the next cut keyframe at/after
			// timeEnd. Blocks before it belong to the GOP that started inside
			// the range: keep them so no frame is lost across segments.
		}

		// Past the gate, so this is the cut keyframe the segment opens on.
		if !cutStartSet {
			cutStart, cutStartSet = blk.Timecode, true
			for _, c := range carried { // the cues still on screen at the cut open this part, for the time they have left
				if end := c.Timecode + c.Duration; end > cutStart {
					opening = append(opening, c)
					c.Timecode, c.Duration = cutStart, end-cutStart
					queue = append(queue, c)
				}
			}
			carried = nil
		}

		srcBlk, isSub := blk, opts.subtitleTracks[blk.TrackNumber]
		newID, ok := opts.remap[blk.TrackNumber]
		if !ok {
			continue
		}
		blk.TrackNumber = newID
		if !firstSet {
			firstTC, firstSet = blk.Timecode, true
		}
		if isSub {
			if want, ok := opts.dropLeadingSub[newID]; ok && blk.Timecode <= firstTC && bytes.Equal(blk.Data, want) {
				delete(opts.dropLeadingSub, newID) // the previous linked part already shows this cue past the seam
				if opts.lastSubs != nil {
					opts.lastSubs[newID] = blk // still this part's cue at its end, for the part after
				}
				continue
			}
			if opts.walk != nil {
				shown = append(shown, srcBlk)
			}
			if opts.lastSubs != nil {
				opts.lastSubs[newID] = blk
			}
		}

		// The timecode stays the SOURCE's until flush: the output's zero is not
		// known yet when the cut is keyframe-aligned (see base above). Nothing
		// below depends on the output timeline - the digests are payload only,
		// the statistics are differences, and the track ends are converted when
		// they are published.
		if opts.contentDigests != nil {
			h := opts.contentDigests[newID]
			if h == nil {
				h = sha256.New()
				opts.contentDigests[newID] = h
			}
			h.Write(blk.Data)
		}
		if opts.contentStats != nil {
			s := opts.contentStats[newID]
			if s == nil {
				s = &trackStats{}
				opts.contentStats[newID] = s
			}
			s.add(&blk)
		}

		if opts.trackEnds != nil {
			s := endStates[newID]
			if s == nil {
				s = &trackEndState{}
				endStates[newID] = s
			}
			if s.hasPrev && blk.Timecode > s.prevTC {
				if g := blk.Timecode - s.prevTC; s.minGap == 0 || g < s.minGap {
					s.minGap = g
				}
			}
			s.prevTC, s.hasPrev = blk.Timecode, true
			if blk.Timecode > s.maxTC {
				s.maxTC = blk.Timecode
				s.lastDur = blk.Duration
			}
		}

		if clusterTS < 0 {
			clusterTS = blk.Timecode
		}
		if blk.Timecode-clusterTS >= clusterMs && len(cluster) > 0 {
			if err := injectSubs(blk.Timecode); err != nil {
				return err
			}
			if err := flush(); err != nil {
				return err
			}
			clusterTS = blk.Timecode
		}

		if err := injectSubs(blk.Timecode); err != nil {
			return err
		}
		cluster = append(cluster, blk)
	}

	if err := injectSubs(1 << 62); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	recordEnds()
	return nil
}

// noCutKeyframeErr reports a range that contains blocks but no video keyframe
// to start on: writing it would either drop the content silently (empty part)
// or produce corrupt video until the next real keyframe, so it is an explicit
// error instead.
func noCutKeyframeErr(startMs int64) error { return noCutKeyframeError{startMs} }

// noCutKeyframeError is noCutKeyframeErr's refusal, typed so a chapter split can name the chapter instead.
type noCutKeyframeError struct{ startMs int64 }

func (e noCutKeyframeError) Error() string {
	return fmt.Sprintf("no video keyframe at/after %dms: cannot start a decodable segment there (video keyframes are sparse in the source); adjust the range to a keyframe", e.startMs)
}

func identityRemap(tracks []mkv.Track) map[uint64]uint64 {
	remap := make(map[uint64]uint64, len(tracks))
	for _, t := range tracks {
		remap[t.ID] = t.ID
	}
	return remap
}

// mergeSource is one input to streamMergeToWriter: a file, its TimecodeScale,
// and the map of its source track numbers to the output track numbers to keep.
type mergeSource struct {
	path  string
	scale int64
	remap map[uint64]uint64
}

// trackStats accumulates one output track's media statistics while streaming,
// for the convention statistics tags (BPS/DURATION/NUMBER_OF_FRAMES/…).
type trackStats struct {
	bytes   int64
	frames  int64
	minTC   int64
	maxTC   int64
	lastDur int64
	seen    bool
}

func (s *trackStats) add(b *mkv.Block) {
	s.bytes += int64(len(b.Data))
	s.frames++
	if !s.seen || b.Timecode < s.minTC {
		s.minTC = b.Timecode
	}
	if !s.seen || b.Timecode > s.maxTC {
		s.maxTC = b.Timecode
		s.lastDur = b.Duration
	}
	s.seen = true
}

// durationMs returns the track's media duration estimate: last frame start −
// first frame start, plus the last frame's explicit duration when it has one.
func (s *trackStats) durationMs() int64 {
	if !s.seen {
		return 0
	}
	return s.maxTC - s.minTC + s.lastDur
}

// streamMergeToWriter k-way merges the wanted blocks of several sources by
// timecode and writes them as time-bounded clusters. It holds only one block
// per source plus the current cluster in memory -- bounded regardless of file
// size, so it is safe for very large inputs (unlike collecting + sorting every
// block). Block.Timecode is in milliseconds for every source, so cross-source
// comparison needs no scale normalisation. The returned map holds each output
// track's accumulated statistics (keyed by output track number).
func streamMergeToWriter(ctx context.Context, mw *writer.MKVWriter, outScale int64, fs *mkv.FS, sources []mergeSource, progress mkv.ProgressFunc) (map[uint64]*trackStats, error) {
	type state struct {
		br   *reader.BlockReader
		f    mkv.ReadSeekCloser
		head mkv.Block
		ok   bool
	}
	states := make([]*state, len(sources))
	defer func() {
		for _, s := range states {
			if s != nil && s.f != nil {
				s.f.Close()
			}
		}
	}()

	// Aggregated progress: each source reports its own byte position; the
	// caller sees the sum over the total size of all sources.
	var progTotal int64
	var progPositions []int64
	if progress != nil {
		progPositions = make([]int64, len(sources))
		for _, src := range sources {
			if st, _ := fs.DoStat(src.path); st != nil {
				progTotal += st.Size()
			}
		}
	}

	advance := func(i int) error {
		st := states[i]
		for {
			blk, err := st.br.Next()
			if err == io.EOF {
				st.ok = false
				return nil
			}
			if err != nil {
				return err
			}
			newID, ok := sources[i].remap[blk.TrackNumber]
			if !ok {
				continue
			}
			blk.TrackNumber = newID
			st.head, st.ok = blk, true
			return nil
		}
	}

	for i, src := range sources {
		f, err := fs.DoOpen(src.path)
		if err != nil {
			return nil, err
		}
		br, err := reader.NewBlockReader(f, src.scale)
		if err != nil {
			f.Close()
			return nil, err
		}
		if progress != nil {
			idx := i
			br.SetProgress(func(p, _ int64) {
				progPositions[idx] = p
				var sum int64
				for _, v := range progPositions {
					sum += v
				}
				progress(sum, progTotal)
			}, progTotal)
		}
		states[i] = &state{br: br, f: f}
		if err := advance(i); err != nil {
			return nil, err
		}
	}

	var cluster []mkv.Block
	clusterTS := int64(-1)
	flush := func() error {
		if len(cluster) == 0 {
			return nil
		}
		err := mw.WriteClusterWithCues(clusterTS, outScale, cluster)
		cluster = cluster[:0]
		return err
	}

	stats := make(map[uint64]*trackStats)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Pick the source whose head block has the smallest timecode. Each
		// source is read in file order; for the common single-track-per-source
		// case that order is already sorted, so this reproduces a global sort.
		mi := -1
		for i, st := range states {
			if !st.ok {
				continue
			}
			if mi < 0 || st.head.Timecode < states[mi].head.Timecode {
				mi = i
			}
		}
		if mi < 0 {
			break
		}
		blk := states[mi].head
		if err := advance(mi); err != nil {
			return nil, err
		}
		ts := stats[blk.TrackNumber]
		if ts == nil {
			ts = &trackStats{}
			stats[blk.TrackNumber] = ts
		}
		ts.add(&blk)

		if clusterTS < 0 {
			clusterTS = blk.Timecode
		}
		if blk.Timecode-clusterTS >= clusterSpanMs(outScale) && len(cluster) > 0 {
			if err := flush(); err != nil {
				return nil, err
			}
			clusterTS = blk.Timecode
		}
		cluster = append(cluster, blk)
	}
	return stats, flush()
}

// cuesOnScreenAt returns the cues still on screen at t, the ones a cut at t runs through.
func cuesOnScreenAt(cues []mkv.Block, t int64) []mkv.Block {
	var out []mkv.Block
	for _, c := range cues {
		if c.Timecode < t && c.Timecode+c.Duration > t {
			out = append(out, c)
		}
	}
	return out
}

// subtitleTrackSet returns the source track numbers of c's subtitle tracks.
func subtitleTrackSet(tracks []mkv.Track) map[uint64]bool {
	set := map[uint64]bool{}
	for _, t := range tracks {
		if t.Type == mkv.SubtitleTrack {
			set[t.ID] = true
		}
	}
	return set
}
