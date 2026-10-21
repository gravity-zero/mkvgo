package mp4

import (
	"context"
	"errors"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// subCursor is one bounded, resumable cue scan over the clusters: it holds
// every cue of its track whose block lives in a cluster stamped within
// [baseMs, scanned]. The prefix cursor (baseMs 0) grows from the track start;
// a fast-path island starts mid-file after a far seek and slides forward.
type subCursor struct {
	baseMs  int64          // cluster-timestamp floor the scan started at (0 = track start)
	cues    []subtitle.Cue // raw cues in block order
	next    int64          // cluster offset to resume from; 0 = not started
	scanned int64          // clusters with timestamps up to this ms are consumed
	done    bool           // scanned to EOF
	index   subIndex       // the cues' settled ends and windows, kept as they are added
}

// subScanState is one subtitle track's serving state: the exact prefix scan
// (whole-track .vtt, sequential playback, and the fallback), one sliding
// island for far seeks, and the flag that gates the island path.
type subScanState struct {
	prefix subCursor
	window *subCursor // fast-seek island; replaced when a jump lands before it
	noFast bool       // a duration-less or over-cap cue was observed: prefix only
	err    error      // permanent (non-context) scan error, replayed
	// indexed holds the track's blocks from Options.SubtitleIndex, loaded on
	// the track's first scan; indexOff is set once the index has been set
	// aside for this track (not covered, or found stale), and the walk serves.
	indexed    []reader.IndexedBlock
	indexAsked bool
	indexOff   bool
}

const (
	// subScanStrideMs is one scan increment: progress commits and the
	// caller's ctx is honoured at each stride, so a client disconnect keeps
	// the clusters already walked and a later request resumes.
	subScanStrideMs = 60_000

	// subFastMaxCueDurMs is the longest cue the fast-seek path accounts for:
	// its backward margin. A cue longer than this observed ANYWHERE (the
	// track-head probe, a prefix scan, an island scan) - or any cue without
	// an explicit duration, whose end resolves on an arbitrarily-far
	// successor - drops the track to the always-exact prefix path. A
	// violating cue that only lives in never-scanned regions can escape this
	// net; real subtitle muxing (explicit BlockDurations, cues of seconds)
	// stays exact.
	subFastMaxCueDurMs = 60_000
)

// relTCMaxMs is how far (ms) a block's timecode can sit from its cluster's
// timestamp: the ±32767-tick relative timecode range at the source's scale.
func (p *HLSPlan) relTCMaxMs() int64 {
	if p.tcScale > 0 && p.tcScale < math.MaxInt64/32767 {
		return 32767 * p.tcScale / 1_000_000
	}
	return 32767
}

// subCuesThrough returns the i-th track's cues, complete and end-resolved for
// every cue starting before uptoMs (math.MaxInt64 = the whole track),
// advancing the prefix cursor as far as that requires and no further.
func (p *HLSPlan) subCuesThrough(ctx context.Context, i int, uptoMs int64) ([]subtitle.Cue, error) {
	if p.mp4src {
		return p.mp4SubWindow(ctx, i, math.MinInt64, uptoMs)
	}
	p.subMu[i].Lock()
	cues, err := p.subPrefixLocked(ctx, i, uptoMs)
	p.subMu[i].Unlock()
	if err != nil {
		return nil, err
	}
	// Resolving on the prefix equals resolving on the full track for every
	// cue starting before uptoMs: the scan guarantees their successors
	// (which duration-less ends resolve on) are in the prefix.
	subtitle.SettleCueEnds(cues, 0)
	return cues, nil
}

// mp4SubWindow is an MP4 source's cues meeting [from, to), the text read the first time a window needs it.
func (p *HLSPlan) mp4SubWindow(ctx context.Context, i int, from, to int64) ([]subtitle.Cue, error) {
	p.subMu[i].Lock()
	defer p.subMu[i].Unlock()
	return p.mp4subs[i].window(ctx, p.openSource, from, to)
}

// subPrefixLocked extends the prefix cursor to cover cues starting before
// uptoMs and returns a copy of it. Caller holds subMu[i].
func (p *HLSPlan) subPrefixLocked(ctx context.Context, i int, uptoMs int64) ([]subtitle.Cue, error) {
	st := &p.subScan[i]
	target := uptoMs
	if relMax := p.relTCMaxMs(); target < math.MaxInt64-relMax {
		target += relMax
	}
	if err := p.extendCursorLocked(ctx, i, &st.prefix, target, uptoMs); err != nil {
		return nil, err
	}
	return append([]subtitle.Cue(nil), st.prefix.cues...), nil
}

// subCuesForWindow returns cues sufficient to window [segStart, segEnd):
// from the prefix when it is the cheaper (or the only exact) vehicle, or
// from a seeked island when the request jumps far past the prefix - the
// direct-play property: a cold seek costs O(window), not O(position). The
// island's backward margin covers cues started before the window
// (subFastMaxCueDurMs) and the relative-timecode spread of their blocks.
func (p *HLSPlan) subCuesForWindow(ctx context.Context, i int, segStart, segEnd int64) ([]subtitle.Cue, error) {
	if p.mp4src {
		return p.mp4SubWindow(ctx, i, segStart, segEnd)
	}
	relMax := p.relTCMaxMs()
	target := segEnd
	if target < math.MaxInt64-relMax {
		target += relMax
	}
	neededFrom := segStart - subFastMaxCueDurMs - relMax
	if neededFrom < 0 {
		neededFrom = 0
	}

	p.subMu[i].Lock()
	st := &p.subScan[i]
	prefixCovers := st.prefix.done ||
		(st.prefix.scanned >= target && !subNeedsNextCue(st.prefix.cues, segEnd))
	// The prefix serves when it already covers the window, when the window
	// starts within (or near) it, or when extending it reads no more than a
	// fresh island would - sequential playback stays on the prefix.
	usePrefix := prefixCovers || neededFrom <= st.prefix.scanned ||
		target-st.prefix.scanned <= target-neededFrom
	if !usePrefix && !st.noFast && st.prefix.scanned == 0 && !st.prefix.done {
		// Probe the track head once before trusting a jump: the first stride
		// reveals the track's cue style (explicit durations or not).
		if err := p.extendCursorLocked(ctx, i, &st.prefix, subScanStrideMs, 0); err != nil {
			p.subMu[i].Unlock()
			return nil, err
		}
	}
	if !usePrefix && !st.noFast {
		cues, err := p.subWindowLocked(ctx, i, neededFrom, target, segEnd)
		if err != nil {
			p.subMu[i].Unlock()
			return nil, err
		}
		if !st.noFast {
			p.subMu[i].Unlock()
			return cues, nil // island cues all carry explicit ends
		}
		st.window = nil // a violation surfaced mid-island: prefix from now on
	}
	cues, err := p.subPrefixWindowLocked(ctx, i, segStart, segEnd)
	p.subMu[i].Unlock()
	return cues, err
}

// subPrefixWindowLocked extends the prefix cursor as subPrefixLocked does and returns the cues that can meet
// [segStart, segEnd), their ends settled over the prefix: the window's cost, not the prefix's. Caller holds subMu[i].
func (p *HLSPlan) subPrefixWindowLocked(ctx context.Context, i int, segStart, segEnd int64) ([]subtitle.Cue, error) {
	st := &p.subScan[i]
	target := segEnd
	if relMax := p.relTCMaxMs(); target < math.MaxInt64-relMax {
		target += relMax
	}
	if err := p.extendCursorLocked(ctx, i, &st.prefix, target, segEnd); err != nil {
		return nil, err
	}
	return st.prefix.windowCues(segStart, segEnd), nil
}

// subWindowLocked serves through the sliding island: reuse it while its
// floor covers the needed lookback AND its scan front has reached it  -
// sliding forward is then incremental. A jump landing before the island or
// far past its front seeks a fresh island through the segment index instead
// of dragging the old one across the gap. Caller holds subMu[i].
func (p *HLSPlan) subWindowLocked(ctx context.Context, i int, neededFrom, target, uptoMs int64) ([]subtitle.Cue, error) {
	st := &p.subScan[i]
	if st.window == nil || st.window.baseMs > neededFrom || st.window.scanned < neededFrom {
		k := sort.Search(len(p.bounds), func(k int) bool { return p.bounds[k] > neededFrom }) - 1
		if k < 0 {
			k = 0
		}
		st.window = &subCursor{baseMs: p.bounds[k], next: p.offsets[k], scanned: p.bounds[k]}
	}
	if err := p.extendCursorLocked(ctx, i, st.window, target, uptoMs); err != nil {
		return nil, err
	}
	return append([]subtitle.Cue(nil), st.window.cues...), nil
}

// keep returns err, caching it for the track when a scan again would meet it again: a damaged region or a
// replaced source. A read that failed (a request cancelled, a server or a disk erring once) is never cached.
func (st *subScanState) keep(err error) error {
	var damage *reader.DamageError
	if errors.As(err, &damage) || errors.Is(err, reader.ErrDamagedRegion) || errors.Is(err, ErrSourceChanged) {
		st.err = err
	}
	return err
}

// extendCursorLocked advances cur until it holds every cue starting before
// uptoMs (within its floor) with resolvable ends. Two bounds drive the walk:
// blocks with timecode T live in clusters stamped within the relative-
// timecode range of T, and a duration-less cue's end is the NEXT cue's start
// - the scan keeps going while the last known cue before uptoMs awaits a
// successor. Cancellation between strides keeps the committed progress; only
// permanent errors are cached. Caller holds subMu[i].
func (p *HLSPlan) extendCursorLocked(ctx context.Context, i int, cur *subCursor, target, uptoMs int64) error {
	st := &p.subScan[i]
	if st.err != nil {
		return st.err
	}
	if cur.done || (cur.scanned >= target && !subNeedsNextCue(cur.cues, uptoMs)) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	track := &p.subs[i]
	src, err := p.openSource(ctx)
	if err != nil {
		return err
	}
	defer src.Close()
	start := cur.next
	if start == 0 {
		start = p.offsets[0]
	}
	// Options.SubtitleIndex: the same scan, reading the track's blocks where
	// the index says they are instead of walking the clusters to find them.
	if entries := p.subIndexed(i); entries != nil {
		served, err := p.extendCursorIndexed(ctx, i, cur, src, entries, start, target, uptoMs)
		if served || err != nil {
			return err
		}
		// The index did not hold for this track: the walk below serves, from
		// the cursor as it stood.
	}
	p.winMu.Lock()
	p.stats.SubtitleWalks++
	p.winMu.Unlock()
	past, perr := p.pastDamage(start)
	if perr != nil {
		return st.keep(perr)
	}
	br, err := p.blockReaderAt(src, past)
	if err != nil {
		return st.keep(err)
	}
	defer p.countWalk(&br)()
	p.tolerateDamage(br)
	br.SetTrackDefaultDurations(p.trackDurs)
	br.KeepTracks(track.track.ID)
	var local []subtitle.Cue
	for {
		stopAt := cur.scanned + subScanStrideMs
		if stopAt < cur.scanned {
			stopAt = math.MaxInt64
		}
		br.StopBeforeClusterMs(stopAt)
		for {
			b, err := br.Next()
			if errors.Is(err, reader.ErrClusterLimit) {
				cur.add(local...)
				local = local[:0]
				cur.next = br.ResumeOffset()
				cur.scanned = stopAt
				break
			}
			// Clean end (incl. a truncated/over-declared tail); all cues to this
			// point are collected, so finish cleanly.
			if p.walkEnd(err) {
				cur.add(local...)
				cur.done = true
				return nil
			}
			if err != nil {
				return st.keep(errf("read block: %w", err))
			}
			if b.TrackNumber != track.track.ID {
				continue
			}
			cue, ok, recovered, err := subCueFromBlockRecovered(&track.track, b)
			if err != nil {
				st.err = err // the cue's own bytes: every scan would refuse them again
				return st.err
			}
			p.noteRecovered(recovered)
			if ok {
				if cue.EndMs <= cue.StartMs || cue.EndMs-cue.StartMs > subFastMaxCueDurMs {
					st.noFast = true
				}
				local = append(local, cue)
			}
		}
		if st.noFast && cur.baseMs > 0 {
			return nil // the island is doomed; the caller falls back to the prefix
		}
		if cur.scanned >= target && !subNeedsNextCue(cur.cues, uptoMs) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// subIndexed returns the i-th subtitle track's blocks from the plan's
// subtitle index, nil when there is none, it does not cover the track, or it
// was found stale. Caller holds subMu[i].
func (p *HLSPlan) subIndexed(i int) []reader.IndexedBlock {
	st := &p.subScan[i]
	if p.subIndex == nil || st.indexOff {
		return nil
	}
	if !st.indexAsked {
		st.indexAsked = true
		st.indexed = p.subIndex.TrackBlocks(p.subs[i].track.ID)
		st.indexOff = len(st.indexed) == 0
	}
	return st.indexed
}

// extendCursorIndexed is extendCursorLocked's scan served from the subtitle
// index: stride by stride, it reads the track's blocks that sit at or past
// start in clusters stamped up to the stride's limit - the very blocks the
// walk would have delivered, in the same order - by seeking to each. Every
// block read is checked against what the index recorded (track, timecode);
// one that does not match means the index is not this file's: nothing of the
// stride is kept, the index is set aside for the track and served is false,
// so the caller walks instead. Caller holds subMu[i].
func (p *HLSPlan) extendCursorIndexed(ctx context.Context, i int, cur *subCursor, src io.ReadSeeker,
	entries []reader.IndexedBlock, start, target, uptoMs int64) (served bool, err error) {
	st := &p.subScan[i]
	track := &p.subs[i]
	stale := func() (bool, error) { // the index does not describe this file: the walk serves from now on
		st.indexOff, st.indexed = true, nil
		return false, nil
	}
	walkOnce := func() (bool, error) { return false, nil } // a read failed: the walk serves this request, the index the next
	var br *reader.BlockReader
	defer p.countWalk(&br)()
	var read int64
	var cues cueResolver
	defer func() {
		p.winMu.Lock()
		p.stats.SubtitleIndexedBlocks += read
		p.winMu.Unlock()
	}()
	for {
		stopAt := cur.scanned + subScanStrideMs
		if stopAt < cur.scanned {
			stopAt = math.MaxInt64
		}
		from := start
		if cur.next != 0 {
			from = cur.next
		}
		j := sort.Search(len(entries), func(k int) bool { return entries[k].Pos.ClusterStart >= from })
		var local []subtitle.Cue
		for ; j < len(entries); j++ {
			if err := ctx.Err(); err != nil {
				return true, err
			}
			e := &entries[j]
			if err := cues.resolve(countedSource{src, &p.walkRead}, e); err != nil {
				return walkOnce()
			}
			clusterMs, ok := p.clusterMs(e.Pos.ClusterTS)
			if !ok || !e.Pos.Valid() || e.Frames < 1 {
				return stale()
			}
			if clusterMs > stopAt {
				break
			}
			if br == nil {
				if br, err = p.blockReaderFrom(src, e.Pos); err != nil {
					return walkOnce()
				}
				br.SetTrackDefaultDurations(p.trackDurs)
				br.KeepTracks(track.track.ID)
			} else if err := br.SeekTo(e.Pos); err != nil {
				return walkOnce()
			}
			for f := int64(0); f < e.Frames; f++ {
				b, err := br.Next()
				if err != nil {
					return walkOnce()
				}
				if b.TrackNumber != track.track.ID || (f == 0 && b.Timecode != e.TimeMs) {
					return stale()
				}
				cue, ok, recovered, err := subCueFromBlockRecovered(&track.track, b)
				if err != nil {
					return true, err
				}
				p.noteRecovered(recovered)
				if ok {
					if cue.EndMs <= cue.StartMs || cue.EndMs-cue.StartMs > subFastMaxCueDurMs {
						st.noFast = true
					}
					local = append(local, cue)
				}
			}
			read++
		}
		cur.add(local...)
		if j == len(entries) {
			cur.done = true
			return true, nil
		}
		cur.next, cur.scanned = entries[j].Pos.ClusterStart, stopAt
		if st.noFast && cur.baseMs > 0 {
			return true, nil // the island is doomed; the caller falls back to the prefix
		}
		if cur.scanned >= target && !subNeedsNextCue(cur.cues, uptoMs) {
			return true, nil
		}
	}
}

// clusterMs converts a cluster's raw Timestamp to milliseconds with the
// plan's timecode scale; ok is false when it does not fit.
func (p *HLSPlan) clusterMs(raw int64) (ms int64, ok bool) {
	if raw < 0 || p.tcScale <= 0 || raw > math.MaxInt64/p.tcScale {
		return 0, false
	}
	return raw * p.tcScale / 1_000_000, true
}

// subNeedsNextCue reports whether serving cues starting before uptoMs still
// needs a later cue: a duration-less cue resolves its end on the NEXT cue's
// start, however far away, so the scan must reach that successor (or EOF).
// Only the trailing run of latest-start cues can lack one.
func subNeedsNextCue(cues []subtitle.Cue, uptoMs int64) bool {
	if len(cues) == 0 {
		return false
	}
	last := cues[len(cues)-1].StartMs
	for i := len(cues) - 1; i >= 0 && cues[i].StartMs == last; i-- {
		if cues[i].StartMs < uptoMs && cues[i].EndMs <= cues[i].StartMs {
			return true
		}
	}
	return false
}

// Subtitle builds the i-th (0-based) subtitle rendition's WebVTT - the whole
// track as one file. It drives the cue cursor to the end of the source; the
// cues collected stay cached for the windowed segments (and vice versa).
// Options.SubtitleOffsetMs shifts every cue (see subtitle.ShiftCues) before
// it is written.
func (p *HLSPlan) Subtitle(ctx context.Context, i int) ([]byte, error) {
	if i < 0 || i >= len(p.subs) {
		return nil, noResource("subtitle rendition %d out of range (0..%d)", i, len(p.subs)-1)
	}
	if err := p.checkSource(); err != nil {
		return nil, err
	}
	cues, err := p.subCuesThrough(ctx, i, math.MaxInt64)
	if err != nil {
		return nil, err
	}
	cues = subtitle.ShiftCues(cues, p.opts.SubtitleOffsetMs)
	var buf strings.Builder
	if err := subtitle.WriteWebVTT(&buf, cues); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// subtitleSegment builds the i-th rendition's n-th windowed WebVTT segment
// (subN_%05d.vtt) - the same windows the full pass writes, byte-identical.
// Only the prefix of the source the window can draw cues from is scanned, so
// serving segments in playback order reads the source once, incrementally  -
// the first hit costs one bounded window, like a video segment.
//
// Options.SubtitleOffsetMs shifts every cue before windowing (segment
// boundaries apply AFTER the shift, matching the full pass): the fetch range
// is the shift's pre-image (a cue's SOURCE time that will land in [segStart,
// segEnd) once shifted is [segStart-offset, segEnd-offset)), so the right
// source cues are pulled in for any offset, however large.
func (p *HLSPlan) subtitleSegment(ctx context.Context, i, n int) ([]byte, error) {
	if i < 0 || i >= len(p.subs) {
		return nil, noResource("subtitle rendition %d out of range (0..%d)", i, len(p.subs)-1)
	}
	if n < 0 || n >= p.segCount {
		return nil, noResource("subtitle segment %d out of range (0..%d)", n, p.segCount-1)
	}
	if err := p.checkSource(); err != nil {
		return nil, err
	}
	segStart := p.bounds[n]
	var segEnd int64 = math.MaxInt64
	if n+1 < p.segCount {
		segEnd = p.bounds[n+1]
	}
	off := p.opts.SubtitleOffsetMs
	srcStart, srcEnd := segStart, segEnd
	if off != 0 {
		srcStart = segStart - off
		if srcStart < 0 {
			srcStart = 0 // subCuesForWindow clamps its own lookback to 0 too
		}
		if segEnd != math.MaxInt64 {
			srcEnd = segEnd - off
		}
	}
	cues, err := p.subCuesForWindow(ctx, i, srcStart, srcEnd)
	if err != nil {
		return nil, err
	}
	cues = subtitle.ShiftCues(cues, off)
	var window []subtitle.Cue
	for _, cue := range cues {
		if cue.EndMs > segStart && cue.StartMs < segEnd {
			window = append(window, cue)
		}
	}
	var buf strings.Builder
	if err := subtitle.WriteWebVTT(&buf, window); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}
