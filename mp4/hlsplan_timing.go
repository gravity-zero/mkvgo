package mp4

import (
	"context"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
)

// segSample is one sample of an on-demand window: the fragment metadata plus
// its bytes (a single segment's data is held in memory - a few MB).
type segSample struct {
	fragSample
	data []byte
	off  int64 // where the payload sits in the source
	end  int64 // where the block's payload ends in the source
}

// sampleOf frames one walked block as a window sample: its decoded bytes, or when the walk skipped its payload its place in the source.
func sampleOf(pt *planTrack, b mkv.Block, structure bool) (segSample, error) {
	meta := fragSample{ptsMs: b.Timecode, blockPtsMs: b.BlockTimecode, sync: b.Keyframe}
	if structure {
		meta.size = uint32(len(pt.ft.outTrack.mkv.HeaderStripping) + int(b.Size))
		return segSample{fragSample: meta, off: b.DataOffset, end: b.DataOffset + b.Size}, nil
	}
	data, err := pt.ft.outTrack.mkv.DecodePayload(b.Data)
	if err != nil {
		return segSample{}, err
	}
	meta.size = uint32(len(data))
	return segSample{fragSample: meta, data: data, off: b.DataOffset, end: b.DataOffset + b.Size}, nil
}

// maxResumeLookMs bounds how far past a window a walk looks for the keyframe its fill copies.
const maxResumeLookMs = 30_000

// fillStepMs spaces the keyframe copies that stand for video a damaged region destroyed.
const fillStepMs = 1000

// fillDamage keeps a video track's timeline whole across a damaged region:
// a window the region emptied serves copies of the first keyframe past it,
// and a window the region cut short serves copies of its own opening
// keyframe from where its frames stop, one a second up to segEnd. A
// keyframe decodes alone, so the copies show a still image where the video
// was lost; without them a player meets an empty fragment, or a hole, and
// hls.js seeking back over the region loops on the empty fragments. It
// reports whether it filled any window.
func (p *HLSPlan) fillDamage(windows [][]segSample, nextPts []int64, resume []*segSample, segStart, segEnd int64) bool {
	filled := false
	for ti, pt := range p.tracks {
		w := windows[ti]
		if !pt.ft.outTrack.spec.video {
			continue
		}
		switch {
		case len(w) == 0 && resume[ti] != nil:
			windows[ti] = keyframeCopies(*resume[ti], segStart, segEnd)
		case len(w) > 0 && w[len(w)-1].cut && w[0].sync:
			from := lastWalkedMs(w) + max(pt.lastDurTS*1000/int64(pt.ft.timescale), 1)
			if from >= segEnd {
				continue
			}
			w[len(w)-1].cut = false
			windows[ti] = append(w, keyframeCopies(w[0], from, segEnd)...)
		default:
			continue
		}
		nextPts[ti], filled = segEnd, true
	}
	return filled
}

// keyframeCopies is src, a keyframe, repeated every fillStepMs from fromMs up to toMs.
func keyframeCopies(src segSample, fromMs, toMs int64) []segSample {
	var out []segSample
	for t := fromMs; t < toMs; t += fillStepMs {
		c := src
		c.ptsMs, c.blockPtsMs, c.sync, c.cut = t, t, true, false
		out = append(out, c)
	}
	return out
}

// cutSlack is how far before a damaged region a block's payload may end and still be taken as cut by it: the
// children a BlockGroup closes on (a ReferenceBlock, a BlockDuration) sit between its payload and the next element.
const cutSlack = 64

// dropCut removes from a window walked so far the samples a damaged region starting at from cut or swallowed,
// marks the track's last remaining sample as closing on the region, and reports whether the track lost any.
func dropCut(window []segSample, from int64) []segSample {
	k := len(window)
	for k > 0 && window[k-1].end > from-cutSlack {
		k--
	}
	window = window[:k]
	if k > 0 {
		window[k-1].cut = true
	}
	return window
}

// emptyWindowTS is the decode time of a window holding no sample of a track: where the window starts on the track's
// timeline, not past the track's end (a window after the track has ended stays parked at its end).
func emptyWindowTS(timescale uint32, originMs, segStartMs, endTS int64) int64 {
	scale := tsScale(timescale)
	return min(max(scale(segStartMs)-scale(originMs), 0), endTS)
}

// timeSegmentWindow derives one segment window's per-sample dtsTS/ctsTS/durTS
// (mutating window in place) purely from metadata - size, ptsMs, blockPtsMs,
// sync - never the sample bytes, applying fillFragTiming's rule window-local:
// grid-timed audio re-derives each frame's index from its block's stored
// timecode, otherwise samples are DTS-sorted and CTS is the PTS/DTS gap.
// nextPts is the decode time the next window opens on - its lowest PTS, as
// the walks' boundary peek finds it (-1 for the presentation's last window,
// which closes on pt.lastDurTS instead). It
// returns the window's base decode time and whether any sample carries a
// non-zero CTS. Shared by segmentTrack (the window's bytes ride along in a
// parallel slice) and the structure-only I-frame builder (bytes never read
// at all), so both derive byte-identical segment heads from the same math.
//
// startK, when not negative, is the frame index a grid-timed window opens on
// (gridStart); nextK returns the index the window after it opens on, -1 when
// there is none or the track is not grid-timed.
func timeSegmentWindow(window []fragSample, pt *planTrack, nextPts, startK int64) (baseDecodeTS int64, hasCTS bool, nextK int64) {
	nextK = -1
	if len(window) == 0 {
		return pt.ft.durMediaTS, false, nextK
	}
	// gridTS from the DefaultDuration, else recovered from the window's own
	// collapsed laces (the stride is uniform, so every window measures the same
	// value the full pass derives from all samples - parity by construction).
	gridTS := pt.gridTS
	if gridTS == 0 && pt.ft.outTrack.mkv.Type == mkv.AudioTrack {
		gridTS = laceGridTS(pt.ft.outTrack, len(window), func(i int) int64 { return window[i].blockPtsMs }, pt.ft.timescale)
	}
	if gridTS == gridSpread { // the full pass's spreadLaced, window-local: the next window's first block closes the last lace
		scale := tsScale(pt.ft.timescale)
		anchor := scale(pt.firstPtsMs) - scale(window[0].blockPtsMs)
		lastSpan := func(m int64) int64 {
			return lastBlockSpan(scale(pt.laceSecondMs)-scale(pt.firstPtsMs), pt.laceFirstFrames, m)
		}
		spreadLaced(len(window), func(i int) int64 { return window[i].blockPtsMs }, scale, nextPts, lastSpan, func(i int, dts, dur int64) {
			window[i].dtsTS, window[i].durTS, window[i].ctsTS = dts-anchor, dur, 0
		})
		return window[0].dtsTS, false, nextK
	}
	if gridTS > 0 {
		// Grid-timed audio: fillFragTiming's running clock applied to the
		// window, opening on startK when the window before it settled it.
		clock := newGridClock(pt, gridTS)
		if startK >= 0 {
			clock.pin(startK)
		}
		for x := range window {
			window[x].dtsTS = clock.advance(window[x].blockPtsMs) * gridTS
			window[x].ctsTS = 0
			if x > 0 {
				window[x-1].durTS = window[x].dtsTS - window[x-1].dtsTS
				if window[x-1].cut {
					window[x-1].durTS = gridTS
				}
			}
		}
		if nextPts >= 0 {
			nextK = clock.peek(nextPts)
			window[len(window)-1].durTS = nextK*gridTS - clock.k*gridTS
			markReachingNext(window, nextK*gridTS)
		}
		if nextPts < 0 || window[len(window)-1].cut { // the track's final sample (fillFragTiming's grid rule), or one a damaged region cut short
			window[len(window)-1].durTS = gridTS
		}
		return window[0].dtsTS, false, nextK
	}
	scale := tsScale(pt.ft.timescale)
	base := scale(pt.firstPtsMs)
	dts := make([]int64, len(window))
	for x := range window {
		dts[x] = scale(window[x].ptsMs)
	}
	sort.Slice(dts, func(a, b int) bool { return dts[a] < dts[b] })
	strictlyIncreasing(dts)
	for x := range window {
		window[x].dtsTS = dts[x] - base
		off := scale(window[x].ptsMs) - dts[x] + pt.ft.ctsShiftTS
		if off < 0 {
			off = 0 // fillFragTiming's clamp, window-local (same rule, same bytes)
		}
		window[x].ctsTS = int32(off)
	}
	for x := 0; x < len(window)-1; x++ {
		window[x].durTS = dts[x+1] - dts[x]
		if window[x].cut {
			window[x].durTS = pt.lastDurTS
		}
	}
	if last := len(window) - 1; nextPts >= 0 && !window[last].cut {
		window[last].durTS = scale(nextPts) - dts[last]
		markReachingNext(window, scale(nextPts)-base)
	} else { // the track's final sample (fillFragTiming's rule), or one a damaged region cut short
		window[last].durTS = pt.lastDurTS
	}
	return dts[0] - base, windowHasCTS(window), nextK
}

// markReachingNext flags the samples timed at or past nextDTS, where the next window opens, and keeps the last one
// lasting forward: the whole-track pass would have pushed the next window instead.
func markReachingNext(window []fragSample, nextDTS int64) {
	for x := range window {
		window[x].reachesNext = window[x].dtsTS >= nextDTS
	}
	if last := &window[len(window)-1]; last.durTS <= 0 {
		last.durTS = 1
	}
}

// gridClock is fillFragTiming's running frame index for grid-timed audio: re-derived from each block's timecode, never stepping back, one slot per lace frame.
type gridClock struct {
	anchor, gridTS int64
	scale          func(int64) int64
	k, prevBlock   int64
	started        bool
	pinned         bool
}

func newGridClock(pt *planTrack, gridTS int64) gridClock {
	scale := tsScale(pt.ft.timescale)
	return gridClock{anchor: scale(pt.firstPtsMs), gridTS: gridTS, scale: scale}
}

// pin makes the next frame take index k exactly (the value the window before learned).
func (c *gridClock) pin(k int64) { c.k, c.pinned = k, true }

// peek returns the index the next frame of block blockPts would take.
func (c *gridClock) peek(blockPts int64) int64 {
	switch {
	case c.pinned:
		return c.k
	case !c.started:
		return gridIndex(c.scale(blockPts)-c.anchor, c.gridTS)
	case blockPts != c.prevBlock:
		if nk := gridIndex(c.scale(blockPts)-c.anchor, c.gridTS); nk > c.k {
			return nk
		}
		return c.k + 1
	default:
		return c.k + 1
	}
}

// advance assigns the next frame of block blockPts its index.
func (c *gridClock) advance(blockPts int64) int64 {
	c.k = c.peek(blockPts)
	c.prevBlock, c.started, c.pinned = blockPts, true, false
	return c.k
}

// gridStartAt returns the frame index track ti's window n opens on, -1 when no walk settled it yet.
func (p *HLSPlan) gridStartAt(n, ti int) int64 {
	p.segMu.Lock()
	defer p.segMu.Unlock()
	if n < 0 || n >= len(p.gridStart) || p.gridStart[n] == nil {
		return -1
	}
	return p.gridStart[n][ti]
}

// learnGridStart records where track ti's window n opens on the grid; the first value kept, so a window's bytes never change between requests.
func (p *HLSPlan) learnGridStart(n, ti int, k int64) {
	if k < 0 || n < 0 || n >= p.segCount {
		return
	}
	p.segMu.Lock()
	defer p.segMu.Unlock()
	for len(p.gridStart) <= n {
		p.gridStart = append(p.gridStart, nil)
	}
	if p.gridStart[n] == nil {
		p.gridStart[n] = make([]int64, len(p.tracks))
		for i := range p.gridStart[n] {
			p.gridStart[n][i] = -1
		}
	}
	if p.gridStart[n][ti] < 0 {
		p.gridStart[n][ti] = k
	}
}

// gridJitter reports whether a window's blocks leave the grid: a timecode stepping back or jumping ahead of the next slot.
func gridJitter(window []segSample, pt *planTrack) bool {
	clock := newGridClock(pt, pt.gridTS)
	for x := range window {
		if x > 0 && window[x].blockPtsMs != window[x-1].blockPtsMs {
			if raw := gridIndex(clock.scale(window[x].blockPtsMs)-clock.anchor, clock.gridTS); raw != clock.k+1 {
				return true
			}
		}
		clock.advance(window[x].blockPtsMs)
	}
	return false
}

// warmGridStart winds track ti's grid clock over the window before n, header-only, to the index window n opens on (-1 when it cannot).
func (p *HLSPlan) warmGridStart(ctx context.Context, n, ti int, first int64) (int64, error) {
	pt := p.tracks[ti]
	end := p.trackPosAt(n)[ti]
	if n <= 0 || !end.Valid() {
		return -1, nil
	}
	clock := newGridClock(pt, pt.gridTS)
	err := p.walkBlocks(ctx, p.offsets[n-1], end.Off, true, func(b mkv.Block, bpt *planTrack) (bool, error) {
		if bpt == pt {
			clock.advance(b.BlockTimecode)
		}
		return true, nil
	})
	if err != nil || !clock.started {
		return -1, err
	}
	return clock.peek(first), nil
}

// timeWindow times one rendition's window as a media segment's track fragment:
// fillFragTiming's DTS derivation applied to the window, anchored on the
// track's global first PTS, the boundary lookahead supplying the final
// sample's duration. An empty window (the track ended before the presentation)
// becomes an empty trun with the decode time parked at the stream end.
// Metadata only, so the buffered and the streamed paths share it verbatim.
func (p *HLSPlan) timeWindow(ti, n int, window []segSample, nextPts, startK int64) trackSegment {
	pt := p.tracks[ti]
	seg := trackSegment{trackID: pt.ft.outTrack.mp4ID}
	metas := make([]fragSample, len(window))
	for x := range window {
		metas[x] = window[x].fragSample
	}
	var nextK int64
	seg.baseDecodeTS, seg.hasCTS, nextK = timeSegmentWindow(metas, pt, nextPts, startK)
	if len(window) == 0 {
		seg.baseDecodeTS = emptyWindowTS(pt.ft.timescale, pt.firstPtsMs, p.bounds[n], pt.ft.durMediaTS)
	}
	if nextK >= 0 {
		p.learnGridStart(n+1, ti, nextK) // where the next window's clock opens, as the full pass has it
	}
	if len(window) > 0 {
		for x := range window {
			window[x].fragSample = metas[x]
			seg.dataLen += int64(window[x].size)
		}
		seg.samples = metas
	}
	return seg
}

// gridStartFor is the grid clock track ti's window n opens on: the value the
// window before learned, else - when the window's timecodes leave the grid, so
// a transient may straddle its start - wound up over the window before, else
// its first block's slot; kept either way, so a rebuild serves the same bytes.
// -1 when the track is not grid-timed.
func (p *HLSPlan) gridStartFor(ctx context.Context, n, ti int, window []segSample) (int64, error) {
	k := p.gridStartAt(n, ti)
	pt := p.tracks[ti]
	if k >= 0 || pt.gridTS <= 0 || len(window) == 0 {
		return k, nil
	}
	if gridJitter(window, pt) {
		var err error
		if k, err = p.warmGridStart(ctx, n, ti, window[0].blockPtsMs); err != nil {
			return -1, err
		}
	} else {
		clock := newGridClock(pt, pt.gridTS)
		k = clock.peek(window[0].blockPtsMs) // the first block's own slot, where a cold clock opens
	}
	p.learnGridStart(n, ti, k)
	return p.gridStartAt(n, ti), nil // the value kept, should another build have learned one first
}
