package mp4

import (
	"context"
	"errors"
	"io"
	"sort"

	"github.com/gravity-zero/mkvgo/internal/poison"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// walkWindow reads the n-th window of EVERY rendition in ONE pass and returns
// each track's samples plus the lowest PTS past the window (-1 when the track
// ends inside it) - the boundary lookahead the fragment timing needs, found
// by nextPtsPeek.
//
// One pass, not one per rendition, because the source cannot give a rendition
// its bytes without handing over the others': the tracks are interleaved block
// by block, and in a real source EVERY payload (a ~10 KiB video frame, a ~2 KiB
// audio frame) sits under the reader's seek-vs-read threshold, so a track filter
// skips none of them. A walk therefore reads 100% of a window whichever slice of
// it it serves - the video's ~77%, an audio track's ~11%. A viewer pulls video
// AND audio for the same instant, so serving them through separate walks read
// the window twice to serve it once. Building every rendition from the single
// pass that had to read all those bytes anyway is what makes the source read
// once. (The cluster prefix is NOT where the cost was: real clusters run ~2s,
// so a 6s segment SPANS several of them - there is no prefix to re-read.)
//
// Cursor semantics per track, exactly like the full pass: a track's window
// starts at its first sample (decode order) with PTS >= segStart and ends just
// before its first sample with PTS >= segEnd. Everything in between belongs to
// it whatever its PTS - open-GOP leading B-frames ride with the keyframe that
// follows them in presentation but precedes them in decode. Membership keys on
// the enclosing block's stored timecode, so a lace is never split. The latch is
// held per track (a shared one would hand a track blocks its own cursor has not
// reached yet, and change its bytes), and the walk runs until EVERY track has
// crossed the end - one track's window can hold blocks stored after another
// track already left the window.
//
// structure asks for a structure-only walk: no video payload is read, each
// sample records where its bytes sit instead (segSample.off), and no arena is
// kept; keepAudio then still reads the other tracks' payloads (a light
// window's audio is served from the table rather than by a second pass).
func (p *HLSPlan) walkWindow(ctx context.Context, n int, segStart, segEnd int64, structure, keepAudio bool) ([][]segSample, []int64, windowInPlace, error) {
	none := windowInPlace{track: -1}
	src, err := p.openSource(ctx)
	if err != nil {
		return nil, nil, none, err
	}
	defer src.Close()

	index := make(map[uint64]int, len(p.tracks))
	keep := make([]uint64, 0, len(p.tracks))
	for i, pt := range p.tracks {
		index[pt.ft.outTrack.mkv.ID] = i
		keep = append(keep, pt.ft.outTrack.mkv.ID)
	}

	// Every track's own opening block known and far apart: a block-ordered
	// source, read track by track (see trackPos).
	if starts := p.trackPosAt(n); p.scatteredWindow(n, starts) {
		windows, nextPts, err := p.walkScatteredWindow(ctx, src, n, starts, keep, segStart, segEnd, structure, keepAudio)
		return windows, nextPts, none, err
	}

	// The window's opening block, when a previous walk revealed it: the FIRST
	// block of any track past segStart, which necessarily precedes (or is) every
	// track's own opening block - so resuming there loses no track's samples.
	// The Cues can only name the enclosing cluster; on a source whose clusters
	// are long enough to hold the whole window, this is what keeps the prefix
	// ahead of it from being read. A miss simply walks from the cluster.
	var br *reader.BlockReader
	defer p.countWalk(&br)()
	var ahead [][]aheadBlock
	from := p.segPosAt(n)
	cold, err := p.pastDamage(p.coldStart(n)) // a seat inside a damaged region already met starts past it
	if err != nil {
		return nil, nil, none, err
	}
	if from.Valid() {
		p.hintWindow(src, n, from.Off)
		br, err = p.blockReaderFrom(src, from)
	} else {
		p.hintWindow(src, n, cold)
		br, err = p.blockReaderAt(src, cold)
	}
	if err != nil {
		return nil, nil, none, err
	}
	p.tolerateDamage(br)
	br.SetTrackDefaultDurations(p.trackDurs)
	br.KeepTracks(keep...)
	if !from.Valid() && cold < p.offsets[n] { // the clusters before the window's own, header-only, then the window's
		if ahead, err = p.coldAhead(ctx, src, br, n, index, segStart, structure, keepAudio); err != nil {
			return nil, nil, none, err
		}
		p.noteSkipped(br)
		own, perr := p.pastDamage(p.offsets[n])
		if perr != nil {
			return nil, nil, none, perr
		}
		if err = br.RestartAt(own); err != nil {
			return nil, nil, none, err
		}
	}
	br.SetHeaderOnly(structure)
	if keepAudio {
		br.SetHeaderOnlyTracks(p.tracks[p.videoIndex()].ft.outTrack.mkv.ID)
	}

	// The video is most of a window's bytes: its blocks are read straight
	// into the buffer its segment is served from (see windowInPlace), not
	// into a buffer each and then copied there.
	inPlace := none
	if !structure {
		inPlace = p.newWindowInPlace(n)
		for ti := range ahead { // a video block ahead of its cluster (never seen) would sit outside the arena
			if len(ahead[ti]) > 0 && p.tracks[ti].ft.outTrack.spec.video {
				inPlace = none
			}
		}
	}
	if inPlace.buf != nil {
		vid := p.tracks[inPlace.track].ft.outTrack.mkv.ID
		br.SetBlockBuffer(func(track uint64, size int) []byte {
			if track != vid {
				return nil
			}
			return inPlace.next(size)
		})
	}

	windows := make([][]segSample, len(p.tracks))
	nextPts := make([]int64, len(p.tracks))
	peeks := make([]nextPtsPeek, len(p.tracks))
	started := make([]bool, len(p.tracks))
	crossed := make([]bool, len(p.tracks))
	opened := make([]bool, len(p.tracks))  // the track's own opening block is recorded
	waiting := make([]bool, len(p.tracks)) // waited for past segEnd: not a track past its own end
	seen := make([]int64, len(p.tracks))   // the track's last block timecode met by this walk
	remaining := 0
	for i := range p.tracks {
		nextPts[i], seen[i] = -1, -1
		started[i] = n == 0
		waiting[i] = !p.trackEndedBefore(i, segStart)
		if waiting[i] {
			remaining++
		}
	}
	for ti := range ahead { // the window's blocks stored before its cluster open their tracks
		for k, a := range ahead[ti] {
			if k == 0 {
				p.learnSegPos(n, a.pos)
				p.learnTrackPos(n, ti, a.pos)
				started[ti], opened[ti] = true, true
			}
			s, err := sampleOf(p.tracks[ti], a.b, structure && (!keepAudio || p.tracks[ti].ft.outTrack.spec.video))
			if err != nil {
				return nil, nil, none, err
			}
			windows[ti] = append(windows[ti], s)
		}
	}
	peeking := 0
	farOff, triedFar, crossedAny := p.farWindowOff(n), false, false
	skips := len(br.SkippedRanges())
	needKey := make([]bool, len(p.tracks))      // a video track past a damaged region: its frames reference what was lost
	resume := make([]*segSample, len(p.tracks)) // a video track's first keyframe past a region, met past segEnd
	seekKey := make([]bool, len(p.tracks))      // past segEnd, still looking for that keyframe
	if p.seatedPastDamage(n) {
		for ti, pt := range p.tracks {
			needKey[ti] = pt.ft.outTrack.spec.video
		}
	}
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return nil, nil, none, err
		}
		b, err := br.Next()
		if k := len(br.SkippedRanges()); k > skips { // passed over a damaged region: what it cut goes, the video waits for a keyframe
			for _, r := range br.SkippedRanges()[skips:] {
				for ti := range windows {
					windows[ti] = dropCut(windows[ti], r.From)
				}
			}
			for ti, pt := range p.tracks {
				needKey[ti] = needKey[ti] || pt.ft.outTrack.spec.video
			}
			skips, inPlace = k, none
		}
		if p.walkEnd(err) { // clean end, incl. a truncated/over-declared tail
			for ti := range p.tracks {
				if !crossed[ti] {
					p.learnTrackEnd(ti, maxInt64(seen[ti], segStart))
				}
			}
			break
		}
		if err != nil {
			return nil, nil, none, errf("read block: %w", err)
		}
		if farOff > 0 && crossedAny && !triedFar && p.walkedFar(from, br.Pos(), farOff) { // a track still missing far past the window: stored after the video?
			triedFar = true
			for ti := range p.tracks {
				if !waiting[ti] || crossed[ti] {
					continue
				}
				far, next, ok, err := p.farTrackWindow(ctx, n, ti, segStart, segEnd, structure && (!keepAudio || p.tracks[ti].ft.outTrack.spec.video), lastWalkedMs(windows[ti]), lastWalkedOff(windows[ti]), br.Pos().Off)
				var near farNearError
				if errors.As(err, &near) {
					farOff, triedFar = near.at, false // look again once the walk reaches the region
					break
				}
				if err != nil {
					return nil, nil, none, err
				}
				if !ok {
					continue
				}
				windows[ti] = append(windows[ti], far...)
				crossed[ti], peeks[ti] = true, nextPtsPeek{cross: next, min: next}
				remaining--
				if ti == inPlace.track {
					inPlace = none // its samples were not read into the arena
				}
			}
			if remaining == 0 {
				break
			}
		}
		ti, ok := index[b.TrackNumber]
		if !ok {
			continue
		}
		if b.BlockTimecode > seen[ti] {
			seen[ti] = b.BlockTimecode
		}
		if crossed[ti] {
			if seekKey[ti] && (b.Keyframe || b.BlockTimecode > segEnd+maxResumeLookMs) {
				if b.Keyframe {
					k, err := sampleOf(p.tracks[ti], b, structure && (!keepAudio || p.tracks[ti].ft.outTrack.spec.video))
					if err != nil {
						return nil, nil, none, err
					}
					k.data = append([]byte(nil), k.data...) // the reader reuses a payload's buffer once the block is not kept
					resume[ti] = &k
				}
				seekKey[ti] = false
				remaining--
			}
			if peeks[ti].observe(b.Timecode, b.Keyframe) {
				remaining--
				peeking--
			}
			continue
		}
		if !started[ti] {
			if b.BlockTimecode < segStart {
				continue // interleaved leftovers of the previous window
			}
			started[ti] = true
			p.learnSegPos(n, br.Pos()) // first past segStart: where this window opens
		}
		if !opened[ti] {
			opened[ti] = true
			p.learnTrackPos(n, ti, br.Pos()) // this track's own first block of the window
		}
		pt := p.tracks[ti]
		if !pt.ft.outTrack.spec.video {
			p.learnAhead(br.Pos(), b)
		}
		if b.BlockTimecode >= segEnd {
			if needKey[ti] && len(windows[ti]) == 0 {
				if b.Keyframe {
					k, err := sampleOf(pt, b, structure && (!keepAudio || pt.ft.outTrack.spec.video))
					if err != nil {
						return nil, nil, none, err
					}
					k.data = append([]byte(nil), k.data...) // the reader reuses a payload's buffer once the block is not kept
					resume[ti] = &k
				} else {
					seekKey[ti] = true // the walk goes on, header by header, to the keyframe the copies are made of
					remaining++
				}
			}
			crossed[ti], crossedAny = true, true
			peeks[ti] = startPeek(b.Timecode, pt.ft.outTrack.spec.video)
			switch {
			case !waiting[ti]:
				peeks[ti].left = 0 // not waited for: its crossing changes nothing
			case peeks[ti].pending():
				peeking++
			default:
				remaining--
			}
			if remaining == peeking {
				br.SetHeaderOnly(true) // only peeks remain: timecodes, never payloads
			}
			p.learnSegPos(n+1, br.Pos()) // first past segEnd: where the next one opens
			p.learnTrackPos(n+1, ti, br.Pos())
			continue
		}
		if needKey[ti] {
			if !b.Keyframe {
				continue
			}
			needKey[ti] = false
		}
		s, err := sampleOf(pt, b, structure && (!keepAudio || pt.ft.outTrack.spec.video))
		if err != nil {
			return nil, nil, none, err
		}
		if ti == inPlace.track {
			s.data = inPlace.keep(s.data)
		}
		windows[ti] = append(windows[ti], s)
	}
	for ti := range peeks {
		if crossed[ti] {
			nextPts[ti] = peeks[ti].min
		}
	}
	if segEnd < 1<<62 && p.fillDamage(windows, nextPts, resume, segStart, segEnd) {
		inPlace = none
	}
	return windows, nextPts, inPlace, nil
}

// reorderLookahead bounds the blocks a peek reads past a boundary (no codec reorders deeper).
const reorderLookahead = 16

// nextPtsPeek finds the next window's lowest PTS: the crossing block's or a leading picture's after it.
type nextPtsPeek struct {
	cross, min int64
	left       int
}

// startPeek opens a peek on the crossing block; only video reorders, so any other track's is over at once.
func startPeek(crossPts int64, video bool) nextPtsPeek {
	p := nextPtsPeek{cross: crossPts, min: crossPts}
	if video {
		p.left = reorderLookahead
	}
	return p
}

// observe folds one block in and reports whether it ended the peek: the first trailing picture, a keyframe or the bound.
func (p *nextPtsPeek) observe(pts int64, keyframe bool) bool {
	if p.left <= 0 {
		return false
	}
	if pts < p.min {
		p.min = pts
	}
	if pts > p.cross || keyframe {
		p.left = 0
	} else {
		p.left--
	}
	return p.left == 0
}

func (p *nextPtsPeek) pending() bool { return p.left > 0 }

// windowNextPts runs the boundary peek over a window held whole, so the I-frame builder derives what the walks do.
func windowNextPts(window []fragSample) int64 {
	peek := startPeek(window[0].ptsMs, true)
	for _, s := range window[1:] {
		if peek.observe(s.ptsMs, s.sync) {
			break
		}
	}
	return peek.min
}

// windowHeadroom is the room kept ahead of a window's video bytes for the
// segment head (styp + moof + mdat header): 16 bytes a sample at most, so
// some two thousand samples - far more than a segment holds.
const windowHeadroom = 32 << 10

// maxWindowInPlace caps the buffer reserved for one window's video. A window
// is a few megabytes to a few tens; a span past this is a source the Cues
// describe oddly, and is built the ordinary way.
const maxWindowInPlace = 256 << 20

// maxSegmentBytes caps one segment's media, whose buffer is sized from the sample sizes the source declares.
const maxSegmentBytes = 256 << 20

// segmentFits refuses a segment whose declared media passes maxSegmentBytes, before its buffer is allocated.
func segmentFits(dataLen int64) error {
	if dataLen > maxSegmentBytes {
		return errf("a segment declares %d bytes of media, past the %d one may hold: the source's sample sizes are damaged or forged, or SegmentMs is too long for its bitrate", dataLen, maxSegmentBytes)
	}
	return nil
}

// windowInPlace is the buffer one window's video is read into and served
// from. The walk hands its tail to the BlockReader for each video block
// (next); a block the window keeps is then committed (keep) - one it does not
// keep (the previous window's leftovers, the block that crosses into the next
// window) simply has its bytes overwritten by the following block. When every
// kept block was committed in order, the buffer holds the segment's media
// back to back behind the headroom, and buildTrackSegment writes the head in
// front of it: no buffer per block, no copy. Anything else - a block that did
// not fit, a laced block, a track whose stripped header had to be put back -
// marks it broken, and the segment is assembled by copy as before.
type windowInPlace struct {
	track  int    // plan-track index it serves; -1 for none
	buf    []byte // headroom + the committed media
	broken bool
	out    []byte // poisoning on: the last block handed out apart from buf, unkept so far
}

// newWindowInPlace reserves the buffer for window n's video: the source bytes
// between the window's two boundaries bound what it holds. No buffer when the
// plan has no video, when CENC needs the plaintext apart, or when that span
// is unknown or implausible.
func (p *HLSPlan) newWindowInPlace(n int) windowInPlace {
	w := windowInPlace{track: -1}
	vi := p.videoIndex()
	if vi >= len(p.tracks) || !p.tracks[vi].ft.outTrack.spec.video || p.opts.CENC != nil {
		return w
	}
	span := p.windowSpan(n)
	if span <= 0 || span > maxWindowInPlace {
		return w
	}
	// The span counts every track; the arena keeps only the video: the
	// others' lightest share seen comes off.
	if other := p.otherEstimate(n); other > 0 && other < span {
		span -= other
	}
	w.track = vi
	w.buf = make([]byte, windowHeadroom, windowHeadroom+int(span))
	return w
}

// otherEstimate is the fewest bytes window n's non-video renditions should run to (0 when nothing is known).
func (p *HLSPlan) otherEstimate(n int) int64 {
	p.winMu.Lock()
	rate := p.otherRate
	p.winMu.Unlock()
	return int64(rate * float64(p.windowMs(n)))
}

// windowMs is window n's presentation length in ms, 0 when unknown.
func (p *HLSPlan) windowMs(n int) int64 {
	switch {
	case n+1 < len(p.bounds):
		return p.bounds[n+1] - p.bounds[n]
	case n < len(p.durs):
		return int64(p.durs[n] * 1000)
	}
	return 0
}

// next returns the uncommitted tail of the buffer for a block of size bytes,
// or nil when it does not fit.
func (w *windowInPlace) next(size int) []byte {
	if w.broken || size <= 0 || size > cap(w.buf)-len(w.buf) {
		return nil
	}
	if poison.On { // the block lands apart, the one handed out before and not kept is poisoned
		poison.Fill(w.out)
		w.out = make([]byte, size)
		return w.out
	}
	return w.buf[len(w.buf) : len(w.buf)+size]
}

// keep commits a block the window keeps. data must be the slice next handed
// out for it; if it is anything else the media is no longer back to back in
// the buffer.
func (w *windowInPlace) keep(data []byte) []byte {
	if w.broken || w.buf == nil || len(data) == 0 {
		return data
	}
	if poison.On && len(w.out) > 0 && &w.out[0] == &data[0] && len(data) <= cap(w.buf)-len(w.buf) {
		w.out = nil
		data = append(w.buf, data...)[len(w.buf):]
	}
	if tail := w.buf[len(w.buf):cap(w.buf)]; len(data) > len(tail) || &tail[0] != &data[0] {
		w.broken = true
		return data
	}
	w.buf = w.buf[:len(w.buf)+len(data)]
	return data
}

// media returns the committed media, or nil when the buffer cannot serve the
// segment in place.
func (w *windowInPlace) media() []byte {
	if w.broken || w.buf == nil {
		return nil
	}
	return w.buf[windowHeadroom:]
}

// walkScatteredWindow reads the n-th window one track at a time, each from
// its own opening block (starts[ti]) with the reader filtered to that track,
// stopping on the track's first block past segEnd. The cursor semantics are
// walkWindow's: the opening block is the track's first block with a stored
// timecode >= segStart, everything after it up to the crossing block belongs
// to the window whatever its PTS, so the samples - and the segment bytes -
// are identical to the linear walk's.
func (p *HLSPlan) walkScatteredWindow(ctx context.Context, src io.ReadSeeker, n int, starts []reader.BlockPos, keep []uint64, segStart, segEnd int64, structure, keepAudio bool) ([][]segSample, []int64, error) {
	windows := make([][]segSample, len(p.tracks))
	nextPts := make([]int64, len(p.tracks))
	for ti, pt := range p.tracks {
		nextPts[ti] = -1
		farOff := int64(-1)
		if span := p.windowSpan(n); span > 0 && n+1 < p.segCount {
			farOff = starts[ti].Off + 2*span // two windows past its own opening block without crossing: far
		}
		br, err := p.blockReaderFrom(src, starts[ti])
		if err != nil {
			return nil, nil, err
		}
		defer p.countWalk(&br)()
		p.tolerateDamage(br)
		br.SetTrackDefaultDurations(p.trackDurs)
		// Every track's blocks come back, the others' headers only: they show how far the walk is while this
		// track's next block is nowhere near (stored apart from the video), and cost nothing more to pass over.
		br.KeepTracks(keep...)
		others := make([]uint64, 0, len(keep)-1)
		for _, id := range keep {
			if id != keep[ti] {
				others = append(others, id)
			}
		}
		skip := structure && (!keepAudio || pt.ft.outTrack.spec.video)
		br.SetHeaderOnly(true)
		if !skip {
			br.SetHeaderOnlyTracks(others...) // this track's payloads, the others' headers
		}
		var peek nextPtsPeek
		crossed, triedFar := false, false
		for {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			b, err := br.Next()
			if p.walkEnd(err) {
				break
			}
			if err != nil {
				return nil, nil, errf("read block: %w", err)
			}
			if farOff > 0 && !crossed && !triedFar && p.walkedFar(starts[ti], br.Pos(), farOff) { // its next block lies far past the window: stored after the video?
				triedFar = true
				far, next, ok, err := p.farTrackWindow(ctx, n, ti, segStart, segEnd, skip, lastWalkedMs(windows[ti]), lastWalkedOff(windows[ti]), br.Pos().Off)
				var near farNearError
				if errors.As(err, &near) {
					farOff, triedFar, err = near.at, false, nil // look again once the walk reaches the region
				}
				if err != nil {
					return nil, nil, err
				}
				if ok {
					windows[ti] = append(windows[ti], far...)
					crossed, peek = true, nextPtsPeek{cross: next, min: next}
					break
				}
			}
			if b.TrackNumber != keep[ti] {
				continue
			}
			if crossed {
				if peek.observe(b.Timecode, b.Keyframe) {
					break
				}
				continue
			}
			if !pt.ft.outTrack.spec.video {
				p.learnAhead(br.Pos(), b)
			}
			if b.BlockTimecode >= segEnd {
				crossed = true
				peek = startPeek(b.Timecode, pt.ft.outTrack.spec.video)
				p.learnSegPos(n+1, br.Pos())
				p.learnTrackPos(n+1, ti, br.Pos())
				if !peek.pending() {
					break
				}
				br.SetHeaderOnly(true) // the peek wants timecodes, never payloads
				continue
			}
			s, err := sampleOf(pt, b, skip)
			if err != nil {
				return nil, nil, err
			}
			windows[ti] = append(windows[ti], s)
		}
		if crossed {
			nextPts[ti] = peek.min
		}
	}
	return windows, nextPts, nil
}

// scatteredWindow reports whether the n-th window should be read track by
// track: every track's opening block is known, they lie in different
// clusters, and further apart than one window's worth of bytes. An
// interleaved source never shows that - its tracks open within the same
// cluster or two, less than a window apart when a window spans clusters, and
// in the SAME cluster when a cluster spans windows - while a block-ordered one
// does everywhere but at a block's tail, where the linear walk is cheap
// anyway. On an interleaved source the linear walk stays: reading one track's
// window there reads everything anyway, and two walks would read it twice.
func (p *HLSPlan) scatteredWindow(n int, starts []reader.BlockPos) bool {
	if len(starts) < 2 {
		return false
	}
	lo, hi := starts[0], starts[0]
	for _, s := range starts {
		if !s.Valid() {
			return false
		}
		if s.Off < lo.Off {
			lo = s
		}
		if s.Off > hi.Off {
			hi = s
		}
	}
	if lo.ClusterStart == hi.ClusterStart {
		return false
	}
	span := p.windowSpan(n)
	return span > 0 && hi.Off-lo.Off > span
}

// windowSpan estimates the bytes one window of the source covers, from the
// Cues' cluster offsets around segment n (the largest window met so far when
// n is the last).
func (p *HLSPlan) windowSpan(n int) int64 {
	var span int64
	switch {
	case n+1 < len(p.offsets) && p.offsets[n+1] > p.offsets[n]:
		span = p.offsets[n+1] - p.offsets[n]
	case n > 0 && n < len(p.offsets) && p.offsets[n] > p.offsets[n-1]:
		span = p.offsets[n] - p.offsets[n-1]
	default:
		p.winMu.Lock()
		defer p.winMu.Unlock()
		return p.winPeak
	}
	if p.spanCap > 0 && span > p.spanCap {
		span = p.spanCap // the cued clusters around n lie apart: something uncued sits between, not the window's bytes
	}
	return span
}

// medianSpanOf is the median distance between consecutive cued windows (0: fewer than two): what a window usually
// spans. Eight times it caps windowSpan, so an uncued stretch between two cued clusters never passes for a window.
func medianSpanOf(offsets []int64) int64 {
	gaps := make([]int64, 0, len(offsets))
	for k := 0; k+1 < len(offsets); k++ {
		if d := offsets[k+1] - offsets[k]; d > 0 {
			gaps = append(gaps, d)
		}
	}
	if len(gaps) < 2 {
		return 0
	}
	sort.Slice(gaps, func(a, b int) bool { return gaps[a] < gaps[b] })
	return gaps[len(gaps)/2]
}

// trackPosAt returns the known opening block of every track for the n-th
// window (zero entries where no walk has revealed one).
func (p *HLSPlan) trackPosAt(n int) []reader.BlockPos {
	out := make([]reader.BlockPos, len(p.tracks))
	if n < 0 {
		return out
	}
	p.segMu.Lock()
	defer p.segMu.Unlock()
	if n < len(p.trackPos) {
		copy(out, p.trackPos[n])
	}
	return out
}
