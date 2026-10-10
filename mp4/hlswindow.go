package mp4

import (
	"context"
)

// minWindowCacheBytes is the FLOOR of the default budget, not the whole of it:
// see HLSPlan.budget, which lifts it to twice the largest window the plan has
// actually seen. A fixed number is wrong for somebody by construction - a 1080p
// window runs ~2 MiB while a high-bitrate 2160p one runs ~22 MiB, and a budget
// under one window's size evicts it before the player has collected its audio,
// so the second request re-walks and the whole saving evaporates on exactly the
// biggest files. This floor only decides the small-source case.
const minWindowCacheBytes = 32 << 20

// windowBundle is one segment index's media: the built segment of EVERY
// rendition, produced by the single walk that had to read all of their bytes.
// A rendition's bytes are released the moment they are handed out - the video
// alone is ~76% of a bundle, and holding it after delivery would turn a read
// saving into a heap - and the bundle goes when the last one is collected. What
// survives is only what nobody came for.
type windowBundle struct {
	segs    [][]byte // per plan-track index; nil once that rendition is delivered
	pending int      // renditions not yet handed out
	bytes   int64    // the media still held (delivered renditions no longer count)
	other   int64    // the non-video samples' payload bytes, as the source holds them
}

// windowFlight is a build in progress: whoever asks for the same window while
// it runs waits for it instead of opening a second walk over the same bytes.
// A player fetching video and audio in parallel (hls.js does) lands here - and
// the two requests share one read of the source.
type windowFlight struct {
	done   chan struct{}
	bundle *windowBundle
	err    error
}

// window returns the n-th window's bundle: from the cache when a sibling
// rendition's request already built it, from a build in flight when one is
// racing, and otherwise from a fresh walk. A miss is always safe - it just
// walks the source, exactly as an uncached plan does - which is what keeps
// serving stateless in effect: no request depends on another having happened.
//
// built reports whether this call walked the source itself.
func (p *HLSPlan) window(ctx context.Context, n int) (b *windowBundle, built bool, err error) {
	p.winMu.Lock()
	if b := p.windows[n]; b != nil {
		p.winMu.Unlock()
		return b, false, nil
	}
	if f := p.winFlight[n]; f != nil {
		p.stats.WaitedBuilds++
		p.winMu.Unlock()
		select {
		case <-f.done:
			return f.bundle, false, f.err
		case <-ctx.Done():
			// This caller gave up; the build carries on for the others.
			return nil, false, ctx.Err()
		}
	}
	f := &windowFlight{done: make(chan struct{})}
	if p.winFlight == nil {
		p.winFlight = make(map[int]*windowFlight)
	}
	p.winFlight[n] = f
	p.winMu.Unlock()

	f.bundle, f.err = p.buildWindow(ctx, n)

	p.winMu.Lock()
	delete(p.winFlight, n)
	if f.err == nil {
		p.noteBuild(n, f.bundle)
		p.store(n, f.bundle)
	}
	p.winMu.Unlock()
	close(f.done)
	return f.bundle, true, f.err
}

// buildWindow reads the n-th window once and frames every rendition of it.
func (p *HLSPlan) buildWindow(ctx context.Context, n int) (*windowBundle, error) {
	release, waited, err := acquireWalk(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if waited {
		p.winMu.Lock()
		p.stats.SlotWaits++
		p.winMu.Unlock()
	}
	segStart := p.bounds[n]
	var segEnd int64 = 1<<63 - 1
	if n+1 < p.segCount {
		segEnd = p.bounds[n+1]
	}
	windows, nextPts, inPlace, err := p.walkWindow(ctx, n, segStart, segEnd, false, false)
	if err != nil {
		return nil, err
	}
	if inPlace.buf != nil && inPlace.media() == nil {
		p.winMu.Lock()
		p.stats.ArenaFallbacks++
		p.winMu.Unlock()
	}
	b := &windowBundle{
		segs:    make([][]byte, len(p.tracks)),
		pending: len(p.tracks),
	}
	for ti := range p.tracks {
		if !p.tracks[ti].ft.outTrack.spec.video {
			for _, s := range windows[ti] {
				b.other += int64(s.size)
			}
		}
		var arena []byte
		if ti == inPlace.track && inPlace.media() != nil {
			arena = inPlace.buf
		}
		k, err := p.gridStartFor(ctx, n, ti, windows[ti])
		if err != nil {
			return nil, err
		}
		data, err := p.buildTrackSegment(ti, n, windows[ti], nextPts[ti], k, arena)
		if err != nil {
			return nil, err
		}
		b.segs[ti] = data
		b.bytes += int64(len(data))
	}
	return b, nil
}

// store publishes a bundle and trims the cache to its byte budget, oldest
// first. Caller holds winMu.
func (p *HLSPlan) store(n int, b *windowBundle) {
	if b == nil {
		return
	}
	if p.winBudget < 0 {
		p.stats.DroppedBytes += b.bytes // caching disabled: kept for nobody but the caller
		return
	}
	if p.windows == nil {
		p.windows = make(map[int]*windowBundle)
	}
	if _, dup := p.windows[n]; dup {
		p.stats.DroppedBytes += b.bytes
		return
	}
	// The budget follows the source: a window this big has now been seen, so it
	// must fit - twice over, so the one being collected and the one after it can
	// both be held.
	if b.bytes > p.winPeak {
		p.winPeak = b.bytes
	}
	p.windows[n] = b
	p.winOrder = append(p.winOrder, n)
	p.winBytes += b.bytes
	for p.winBytes > p.budget() && len(p.winOrder) > 0 {
		p.stats.Evictions++
		p.dropLocked(p.winOrder[0])
	}
}

// takeRendition hands out one rendition's segment AND releases it in the same
// breath, under the lock: the caller keeps the slice, the plan lets it go. Not
// releasing on delivery is what would make this a heap instead of a read saving
// - `pending` never reaches zero in the field (a player takes one audio track
// and leaves the other languages), so a bundle waits for the byte budget to
// evict it, and until then it still holds the video it already served: ~76% of
// its bytes, dead weight.
//
// Returns nil when the rendition has already been collected and freed - two
// viewers landing on the same segment at once. The caller then rebuilds it: a
// miss is always safe, which is the property this whole cache rests on.
//
// shared says the bundle was built by another request: the rendition then
// cost this one no walk, and is counted as such.
func (p *HLSPlan) takeRendition(n, ti int, b *windowBundle, shared bool) []byte {
	p.winMu.Lock()
	defer p.winMu.Unlock()
	if b == nil || ti < 0 || ti >= len(b.segs) {
		return nil
	}
	data := b.segs[ti]
	if data == nil {
		return nil
	}
	if shared {
		p.stats.SharedRenditions++
	}
	b.segs[ti] = nil
	b.pending--
	b.bytes -= int64(len(data))
	p.stats.ServedBytes += int64(len(data))
	if p.windows[n] == b {
		p.winBytes -= int64(len(data))
		if b.pending <= 0 || p.consumed(b) {
			p.dropLocked(n)
		}
	} else {
		// The bundle left the cache before this rendition was collected (the
		// budget, or caching off): it was counted as dropped then, and is
		// served after all.
		p.stats.DroppedBytes -= int64(len(data))
	}
	return data
}

// consumed reports whether a window has given a viewer everything a viewer
// takes: its video and ONE audio track. The other language and the subtitles are
// never asked for, so `pending` never reaches zero in the field - a bundle that
// waited for it would sit in the cache until the budget pushed it out, and on a
// source with heavy audio (DTS-HD) those leftovers fill the cache and evict the
// windows that ARE about to be collected: the saving decays as a viewer watches
// on, and a seek loses it outright. Dropping on the consumption profile instead
// of on exhaustion keeps the cache holding only what is actually in flight. A
// viewer who switches language mid-film re-walks one window - rare, and a miss is
// correct by construction.
func (p *HLSPlan) consumed(b *windowBundle) bool {
	videoTaken, hasAudio, audioTaken := true, false, false
	for ti, pt := range p.tracks {
		if ti >= len(b.segs) {
			break
		}
		taken := b.segs[ti] == nil
		if pt.ft.outTrack.spec.video {
			videoTaken = videoTaken && taken
			continue
		}
		hasAudio = true
		audioTaken = audioTaken || taken
	}
	return videoTaken && (!hasAudio || audioTaken)
}

// dropLocked removes a bundle from the cache. Callers already holding its
// segment keep it - only the plan's reference goes. Caller holds winMu.
func (p *HLSPlan) dropLocked(n int) {
	b := p.windows[n]
	if b == nil {
		return
	}
	delete(p.windows, n)
	p.winBytes -= b.bytes
	p.stats.DroppedBytes += b.bytes // what nobody came for
	for i, k := range p.winOrder {
		if k == n {
			p.winOrder = append(p.winOrder[:i], p.winOrder[i+1:]...)
			break
		}
	}
}

// budget is the byte ceiling for the windows this plan holds. Set explicitly, it
// is whatever the caller said. Left at zero it is DERIVED FROM THE SOURCE: twice
// the largest window seen, floored at minWindowCacheBytes. A window that does not
// fit in the budget is evicted before the player has collected the rest of its
// renditions, and the second request re-walks it - so a fixed ceiling silently
// undoes the whole saving on any source whose windows outgrow it, which is
// precisely the big files that need it most. Deriving it means a 1080p plan stays
// small, a 2160p one is covered, and a bitrate nobody has shipped yet is not a
// cliff. Caller holds winMu.
func (p *HLSPlan) budget() int64 {
	if p.winBudget != 0 {
		return p.winBudget
	}
	b := int64(minWindowCacheBytes)
	if twice := 2 * p.winPeak; twice > b {
		b = twice
	}
	return b
}

// HLSPlanStats counts, since the plan was built, what serving its segments
// cost in walks of the source. A Matroska source interleaves its tracks, so a
// window is read once for every rendition of it (see Options.WindowCacheBytes):
// these counters say how often that sharing held, and how often a window was
// read again. An MP4-source plan reads each rendition's samples on their own
// and counts nothing here.
type HLSPlanStats struct {
	// WindowBuilds is the number of walks of the source that built a window;
	// WindowRebuilds, how many of them built a window this plan had already
	// built - a second viewer on the same segment, a rendition asked for after
	// the window was collected or evicted. Each is a full read of the window.
	WindowBuilds   int64
	WindowRebuilds int64
	// SharedRenditions is the number of requests answered from a window
	// another request had built (or was building); WaitedBuilds, those that
	// waited on a build in flight instead of starting a walk of their own.
	SharedRenditions int64
	WaitedBuilds     int64
	// SlotWaits is the number of walks that queued for a process slot
	// (SetMaxConcurrentWalks) before they could read.
	SlotWaits int64
	// ArenaFallbacks is the number of windows whose video buffer, sized on
	// the span less the other tracks' lightest share seen, came out too small
	// and were assembled by copy instead: each one lowers that share.
	ArenaFallbacks int64
	// TableBuilds is the number of structure-only walks that built a window
	// table (Open); TableEvictions, the tables the budgets pushed out;
	// StreamedSegments, the segments written from the source through a table;
	// StreamFallbacks, the segments Open had to build in memory because the
	// plan cannot stream (see HLSPlan.Open).
	TableBuilds      int64
	TableEvictions   int64
	StreamedSegments int64
	StreamFallbacks  int64
	// UndeclaredZlibBlocks is the number of subtitle blocks the plan inflated
	// although their track declares no compression: a muxer compressed the
	// track and lost the ContentEncodings (mkvgo diagnose reports such tracks).
	UndeclaredZlibBlocks int64
	// Evictions is the number of windows the byte budget pushed out before
	// their renditions were collected.
	Evictions int64
	// BuiltBytes is the media the walks framed, ServedBytes what was handed
	// out, DroppedBytes what was built and released without being served.
	BuiltBytes   int64
	ServedBytes  int64
	DroppedBytes int64
	// SubtitleWalks is the number of times a subtitle rendition had to walk
	// the clusters for its cues (each walk reads a minute or more of the
	// file); SubtitleIndexedBlocks, the subtitle blocks read straight from
	// their position through Options.SubtitleIndex. A plan given an index
	// that still counts walks is serving a track the index does not hold, or
	// an index that is not this file's.
	SubtitleWalks         int64
	SubtitleIndexedBlocks int64
}

// Stats returns the plan's window counters. Safe to call while segments are
// being served.
func (p *HLSPlan) Stats() HLSPlanStats {
	p.winMu.Lock()
	defer p.winMu.Unlock()
	return p.stats
}

// learnOtherRate folds one window's non-video payload bytes into the lightest rate seen (otherRate).
func (p *HLSPlan) learnOtherRate(n int, other int64) {
	p.winMu.Lock()
	defer p.winMu.Unlock()
	p.learnOtherRateLocked(n, other)
}

// learnOtherRateLocked is learnOtherRate with winMu held.
func (p *HLSPlan) learnOtherRateLocked(n int, other int64) {
	if ms := p.windowMs(n); ms > 0 {
		if rate := float64(other) / float64(ms); p.otherRate == 0 || rate < p.otherRate {
			p.otherRate = rate
		}
	}
}

// heldBytes is the media the plan's windows hold right now.
func (p *HLSPlan) heldBytes() int64 {
	p.winMu.Lock()
	defer p.winMu.Unlock()
	return p.winBytes
}

// noteBuild counts one walk that built window n. Caller holds winMu, and b
// still holds everything the walk framed.
func (p *HLSPlan) noteBuild(n int, b *windowBundle) {
	p.stats.WindowBuilds++
	p.stats.BuiltBytes += b.bytes
	p.learnOtherRateLocked(n, b.other)
	if p.winBuilt == nil {
		p.winBuilt = make([]uint64, (p.segCount+63)/64)
	}
	if n/64 >= len(p.winBuilt) { // a growing plan has gained segments since
		p.winBuilt = append(p.winBuilt, make([]uint64, n/64+1-len(p.winBuilt))...)
	}
	if p.winBuilt[n/64]&(1<<(n%64)) != 0 {
		p.stats.WindowRebuilds++
	}
	p.winBuilt[n/64] |= 1 << (n % 64)
}
