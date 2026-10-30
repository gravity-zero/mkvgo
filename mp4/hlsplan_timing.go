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
	meta := fragSample{ptsMs: b.Timecode, blockPtsMs: b.BlockTimecode, sync: b.Keyframe, srcOff: b.DataOffset}
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
// span is where a grid-timed window opens and where the next one does
// (gridSpanFor); noGridSpan opens on the first block's own slot and ends on
// nextPts's.
func timeSegmentWindow(window []fragSample, pt *planTrack, nextPts int64, span gridSpan) (baseDecodeTS int64, hasCTS bool) {
	if len(window) == 0 {
		return pt.ft.durMediaTS, false
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
		return window[0].dtsTS, false
	}
	if gridTS > 0 {
		// Grid-timed audio: the full pass's window-by-window rule (gridTime).
		scale := tsScale(pt.ft.timescale)
		anchor := scale(pt.firstPtsMs)
		slot := func(ms int64) int64 { return gridIndex(scale(ms)-anchor, gridTS) }
		if span.end < 0 && nextPts >= 0 {
			span.end = slot(nextPts)
		}
		gridTime(len(window), func(i int) int64 { return window[i].blockPtsMs }, slot, gridTS, span, func(i int, dts, dur int64) {
			window[i].dtsTS, window[i].durTS, window[i].ctsTS = dts, dur, 0
		})
		for x := range window {
			if window[x].cut { // a sample a damaged region cut short
				window[x].durTS = gridTS
			}
		}
		return window[0].dtsTS, false
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
		keepLastForward(window)
	} else { // the track's final sample (fillFragTiming's rule), or one a damaged region cut short
		window[last].durTS = pt.lastDurTS
	}
	return dts[0] - base, windowHasCTS(window)
}

// keepLastForward keeps the window's last sample lasting forward: the whole-track pass would have pushed the next
// window instead.
func keepLastForward(window []fragSample) {
	if last := &window[len(window)-1]; last.durTS <= 0 {
		last.durTS = 1
	}
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

// timeWindow times one rendition's window as a media segment's track fragment:
// fillFragTiming's DTS derivation applied to the window, anchored on the
// track's global first PTS, the boundary lookahead supplying the final
// sample's duration. An empty window (the track ended before the presentation)
// becomes an empty trun with the decode time parked at the stream end.
// Metadata only, so the buffered and the streamed paths share it verbatim.
func (p *HLSPlan) timeWindow(ti, n int, window []segSample, nextPts int64, span gridSpan) trackSegment {
	pt := p.tracks[ti]
	seg := trackSegment{trackID: pt.ft.outTrack.mp4ID}
	metas := make([]fragSample, len(window))
	for x := range window {
		metas[x] = window[x].fragSample
	}
	seg.baseDecodeTS, seg.hasCTS = timeSegmentWindow(metas, pt, nextPts, span)
	if len(window) == 0 {
		seg.baseDecodeTS = emptyWindowTS(pt.ft.timescale, pt.firstPtsMs, p.bounds[n], pt.ft.durMediaTS)
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

// gridSpanFor is where track ti's window n opens and where window n+1 does, as gridTimeWindows has them for the full
// pass; noGridSpan when the track is not grid-timed. A window whose blocks leave the grid reads the window before,
// header-only, and one ending past the next block's own slot reads the window after: three windows at most, never
// the track from its start. Each opening is kept, so a rebuild serves the same bytes.
func (p *HLSPlan) gridSpanFor(ctx context.Context, n, ti int, window []segSample, nextPts int64) (gridSpan, error) {
	pt := p.tracks[ti]
	if pt.gridTS <= 0 || len(window) == 0 {
		return noGridSpan, nil
	}
	scale := tsScale(pt.ft.timescale)
	anchor := scale(pt.firstPtsMs)
	slot := func(ms int64) int64 { return gridIndex(scale(ms)-anchor, pt.gridTS) }
	pts := func(w []segSample) func(int) int64 { return func(i int) int64 { return w[i].blockPtsMs } }
	span := gridSpan{open: p.gridStartAt(n, ti), end: -1}
	if span.open < 0 {
		prevEnd := int64(-1)
		if n > 0 && gridJitter(len(window), pts(window), slot) {
			prev, err := p.gridNeighbour(ctx, n-1, ti)
			if err != nil {
				return noGridSpan, err
			}
			if len(prev) > 0 {
				prevEnd = gridColdEnd(len(prev), pts(prev), slot)
			}
		}
		p.learnGridStart(n, ti, gridOpen(len(window), pts(window), slot, prevEnd))
		span.open = p.gridStartAt(n, ti) // the value kept, should another build have learned one first
	}
	if nextPts < 0 {
		return span, nil
	}
	if span.end = p.gridStartAt(n+1, ti); span.end >= 0 {
		return span, nil
	}
	span.end = slot(nextPts)
	if n+2 < p.segCount && nextPts >= p.bounds[n+2] {
		return span, nil // the next block opens a later window, after one this track holds nothing in
	}
	if coldEnd := gridColdEnd(len(window), pts(window), slot); coldEnd > span.end {
		next, err := p.gridNeighbour(ctx, n+1, ti)
		if err != nil {
			return noGridSpan, err
		}
		if len(next) > 0 {
			span.end = gridOpen(len(next), pts(next), slot, coldEnd)
		}
	}
	p.learnGridStart(n+1, ti, span.end)
	return span, nil
}

// gridNeighbour reads track ti's window m, header-only.
func (p *HLSPlan) gridNeighbour(ctx context.Context, m, ti int) ([]segSample, error) {
	segEnd := int64(1<<63 - 1)
	if m+1 < p.segCount {
		segEnd = p.bounds[m+1]
	}
	windows, _, _, err := p.walkWindow(ctx, m, p.bounds[m], segEnd, true, false)
	if err != nil {
		return nil, err
	}
	return windows[ti], nil
}
