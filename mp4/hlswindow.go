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
func (p *HLSPlan) window(ctx context.Context, n int) (*windowBundle, error) {
	p.winMu.Lock()
	if b := p.windows[n]; b != nil {
		p.winMu.Unlock()
		return b, nil
	}
	if f := p.winFlight[n]; f != nil {
		p.winMu.Unlock()
		select {
		case <-f.done:
			return f.bundle, f.err
		case <-ctx.Done():
			// This caller gave up; the build carries on for the others.
			return nil, ctx.Err()
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
		p.store(n, f.bundle)
	}
	p.winMu.Unlock()
	close(f.done)
	return f.bundle, f.err
}

// buildWindow reads the n-th window once and frames every rendition of it.
func (p *HLSPlan) buildWindow(ctx context.Context, n int) (*windowBundle, error) {
	segStart := p.bounds[n]
	var segEnd int64 = 1<<63 - 1
	if n+1 < p.segCount {
		segEnd = p.bounds[n+1]
	}
	windows, nextPts, err := p.walkWindow(ctx, n, segStart, segEnd)
	if err != nil {
		return nil, err
	}
	b := &windowBundle{
		segs:    make([][]byte, len(p.tracks)),
		pending: len(p.tracks),
	}
	for ti := range p.tracks {
		data, err := p.buildTrackSegment(ti, n, windows[ti], nextPts[ti])
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
	if p.winBudget < 0 || b == nil {
		return // caching disabled
	}
	if p.windows == nil {
		p.windows = make(map[int]*windowBundle)
	}
	if _, dup := p.windows[n]; dup {
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
func (p *HLSPlan) takeRendition(n, ti int, b *windowBundle) []byte {
	p.winMu.Lock()
	defer p.winMu.Unlock()
	if b == nil || ti < 0 || ti >= len(b.segs) {
		return nil
	}
	data := b.segs[ti]
	if data == nil {
		return nil
	}
	b.segs[ti] = nil
	b.pending--
	b.bytes -= int64(len(data))
	if p.windows[n] == b {
		p.winBytes -= int64(len(data))
		if b.pending <= 0 || p.consumed(b) {
			p.dropLocked(n)
		}
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
