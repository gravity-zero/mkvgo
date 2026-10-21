package mp4

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// noteTrackCues records each track's own cue points by time (absolute cluster offsets) and its widest cue spacing (never under a segment's length).
func (p *HLSPlan) noteTrackCues(cues []mkv.CuePoint, segmentStart, segMs int64) {
	p.trackCues = make([][]mkv.CuePoint, len(p.tracks))
	p.trackCueGapMs = make([]int64, len(p.tracks))
	for i, pt := range p.tracks {
		p.trackCueGapMs[i] = segMs
		for _, cue := range cues {
			if cue.Track == pt.ft.outTrack.mkv.ID {
				cue.ClusterPos += segmentStart
				p.trackCues[i] = append(p.trackCues[i], cue)
			}
		}
		own := p.trackCues[i]
		sort.Slice(own, func(a, b int) bool { return own[a].TimeMs < own[b].TimeMs })
		for k := 1; k < len(own); k++ {
			if gap := own[k].TimeMs - own[k-1].TimeMs; gap > p.trackCueGapMs[i] {
				p.trackCueGapMs[i] = gap
			}
		}
	}
}

// trackEndedBefore reports that track ti has no block at or past ms: its last cue lies two cue gaps behind, or a walk met the end of the source without one.
func (p *HLSPlan) trackEndedBefore(ti int, ms int64) bool {
	if ti < len(p.trackCues) && len(p.trackCues[ti]) > 0 && p.trackCues[ti][len(p.trackCues[ti])-1].TimeMs+2*p.trackCueGapMs[ti] < ms {
		return true
	}
	p.segMu.Lock()
	defer p.segMu.Unlock()
	return ti < len(p.trackEndMs) && p.trackEndMs[ti] >= 0 && p.trackEndMs[ti] <= ms
}

// learnTrackEnd records that a walk met the end of the source with track ti's last block at ms.
func (p *HLSPlan) learnTrackEnd(ti int, ms int64) {
	p.segMu.Lock()
	defer p.segMu.Unlock()
	if len(p.trackEndMs) == 0 {
		p.trackEndMs = make([]int64, len(p.tracks))
		for i := range p.trackEndMs {
			p.trackEndMs[i] = -1
		}
	}
	if ms > p.trackEndMs[ti] {
		p.trackEndMs[ti] = ms
	}
}

// tailMark is a cluster of the uncued tail and the time it opens on.
type tailMark struct{ off, ms int64 }

// farTailGapMs is the most a track's tail may open after its last block walked for the tail to still be the window's continuation.
const farTailGapMs = 2000

// lastWalkedOff is the furthest payload offset among a window's samples walked so far (-1: none).
func lastWalkedOff(window []segSample) int64 {
	last := int64(-1)
	for _, s := range window {
		last = maxInt64(last, s.off)
	}
	return last
}

// lastWalkedMs is the latest stored timecode among a window's samples walked so far (-1: none).
func lastWalkedMs(window []segSample) int64 {
	last := int64(-1)
	for _, s := range window {
		last = maxInt64(last, s.blockPtsMs)
	}
	return last
}

// tailRegion is a stretch of uncued clusters: from its first cluster to the next cued one (end -1: to the end of the
// source); once walked to its end (complete), maxMs is the latest block timecode met in it.
type tailRegion struct {
	start, end int64
	firstMs    int64 // the timecode of its first block (-1: not read yet)
	complete   bool
	maxMs      int64
}

// uncuedRegionsOf finds, once, the stretches of clusters the Cues never name: between two cued clusters further apart
// than clusters run (the first one's header says where it ends), and after the last cued one.
func (p *HLSPlan) uncuedRegionsOf(src io.ReadSeeker) []tailRegion {
	p.segMu.Lock()
	known, regions := p.uncuedKnown, p.uncuedRegions
	p.segMu.Unlock()
	if known {
		return regions
	}
	n := len(p.clusters)
	gaps := make([]int64, 0, n)
	for k := 0; k+1 < n; k++ {
		gaps = append(gaps, p.clusters[k+1]-p.clusters[k])
	}
	sort.Slice(gaps, func(a, b int) bool { return gaps[a] < gaps[b] })
	var wide int64 = 1 << 20 // a cued cluster further from the next than twice the usual: something uncued between?
	if len(gaps) > 0 {
		wide = maxInt64(wide, 2*gaps[len(gaps)/2])
	}
	clusterEnd := func(off int64) int64 { // where the cued cluster at off ends (-1: unknown)
		br, err := p.blockReaderAt(src, off)
		if err != nil {
			return -1
		}
		br.SetHeaderOnly(true)
		defer func() { p.walkRead.Add(br.BytesRead()) }()
		if _, err := br.Next(); err != nil {
			return -1
		}
		return br.Pos().ClusterEnd
	}
	opensCluster := func(off int64) bool { // a Cluster element sits at off
		br, err := p.blockReaderAt(src, off)
		if err != nil {
			return false
		}
		br.SetHeaderOnly(true)
		defer func() { p.walkRead.Add(br.BytesRead()) }()
		_, err = br.Next()
		return err == nil
	}
	for k := 0; k < n; k++ {
		last := k+1 == n
		if !last && p.clusters[k+1]-p.clusters[k] <= wide {
			continue
		}
		end := clusterEnd(p.clusters[k])
		if end <= 0 || (!last && end >= p.clusters[k+1]) || !opensCluster(end) {
			continue
		}
		rg := tailRegion{start: end, end: -1, firstMs: -1, maxMs: -1}
		if !last {
			rg.end = p.clusters[k+1]
		}
		regions = append(regions, rg)
	}
	p.segMu.Lock()
	p.uncuedRegions, p.uncuedKnown = regions, true
	p.segMu.Unlock()
	return regions
}

// regionFirstMs is the timecode of the i-th uncued region's first block, read once (math.MaxInt64 when it cannot be).
func (p *HLSPlan) regionFirstMs(src io.ReadSeeker, i int) int64 {
	p.segMu.Lock()
	rg := p.uncuedRegions[i]
	p.segMu.Unlock()
	if rg.firstMs >= 0 {
		return rg.firstMs
	}
	first := int64(math.MaxInt64)
	if br, err := p.blockReaderAt(src, rg.start); err == nil {
		br.SetHeaderOnly(true)
		if b, err := br.Next(); err == nil {
			first = b.BlockTimecode
		}
		p.walkRead.Add(br.BytesRead())
	}
	p.segMu.Lock()
	p.uncuedRegions[i].firstMs = first
	p.segMu.Unlock()
	return first
}

// tailSeat is where a walk of an uncued region opens for segStart: the last of its clusters known to open at or before
// it; a wide region is bisected first (seek, resync to the next cluster, read its timestamp) so a window deep inside
// it is reached in a few probes rather than by walking the region from its start.
func (p *HLSPlan) tailSeat(src io.ReadSeeker, rg tailRegion, n int, segStart int64) int64 {
	p.segMu.Lock()
	seat, bound := rg.start, rg.start
	for _, m := range p.tailMarks {
		if rg.end >= 0 && m.off >= rg.end || m.off < rg.start {
			continue
		}
		if m.ms <= segStart && m.off > seat {
			seat = m.off
		}
		if m.ms > segStart && (bound == rg.start || m.off < bound) {
			bound = m.off // the first cluster known to open after segStart: the seat lies before it
		}
	}
	p.segMu.Unlock()
	span := p.medianSpan
	hi := bound
	if hi == rg.start {
		hi = rg.end
		if hi < 0 {
			if size, err := src.Seek(0, io.SeekEnd); err == nil {
				hi = size
			}
		}
	}
	for span > 0 && hi > seat && hi-seat > 2*span { // a few probes, each a cluster header: cheaper than a walk from seat
		mid := seat + (hi-seat)/2
		off, ms, ok := p.probeCluster(src, mid, hi)
		if !ok || off <= seat {
			hi = mid
			continue
		}
		p.markTail(off, ms)
		if ms <= segStart {
			seat = off
		} else {
			hi = off
		}
	}
	return seat
}

// walkedFar reports that a walk opened at from and now at pos has gone far past its window without the track it waits
// for: beyond farOff, or from outside into a stretch of uncued clusters already known (what it waits for is not ahead).
func (p *HLSPlan) walkedFar(from, pos reader.BlockPos, farOff int64) bool {
	if pos.Off > farOff {
		return true
	}
	p.segMu.Lock()
	defer p.segMu.Unlock()
	for _, rg := range p.uncuedRegions {
		if rg.end >= 0 && rg.end-rg.start <= 2*p.medianSpan {
			continue // a cluster or two the walk passes over anyway
		}
		if from.Off < rg.start && pos.ClusterStart >= rg.start && (rg.end < 0 || pos.ClusterStart < rg.end) {
			return true
		}
	}
	return false
}

// probeCluster finds the first cluster at or after off (before limit) and reads the time it opens on.
func (p *HLSPlan) probeCluster(src io.ReadSeeker, off, limit int64) (at, ms int64, ok bool) {
	if _, err := src.Seek(off, io.SeekStart); err != nil {
		return 0, 0, false
	}
	at, err := reader.ResyncToCluster(src, limit)
	if err != nil || at < 0 {
		return 0, 0, false
	}
	br, err := p.blockReaderAt(src, at)
	if err != nil {
		return 0, 0, false
	}
	br.SetHeaderOnly(true)
	defer func() { p.walkRead.Add(br.BytesRead()) }()
	if _, err := br.Next(); err != nil {
		return 0, 0, false
	}
	ms, ok = p.clusterMs(br.Pos().ClusterTS)
	return at, ms, ok
}

// markTail records a cluster of the uncued tail and the time it opens on.
func (p *HLSPlan) markTail(off, ms int64) {
	p.segMu.Lock()
	defer p.segMu.Unlock()
	if n := len(p.tailMarks); n == 0 || p.tailMarks[n-1].off < off {
		p.tailMarks = append(p.tailMarks, tailMark{off, ms})
	}
}

// farWindowOff is how far past window n's cluster a walk looks for a track before trying the uncued regions (-1: the last window, never).
func (p *HLSPlan) farWindowOff(n int) int64 {
	span := p.windowSpan(n)
	if n+1 >= len(p.offsets) || span <= 0 {
		return -1
	}
	return p.offsets[n] + 3*span
}

// farNearError says the uncued region lies just past where the walk stands (at): the walk goes on to it and looks again there.
type farNearError struct{ at int64 }

func (e farNearError) Error() string { return fmt.Sprintf("uncued region near, at %d", e.at) }

// farState is what a walk of one uncued region found for a window's track.
type farState int

const (
	farElsewhere farState = iota // the region opens later than the window's continuation: the samples lie elsewhere
	farEnded                     // the region ran out (or the source) before the track crossed the window's end
	farCrossed                   // the track crossed the window's end in the region
)

// farTrackWindow collects track ti's samples of window n from the uncued regions, a track stored apart from the video,
// in file order, and reports whether they were there: a region opens at or before the window, or right after lastMs,
// the track's last block walked so far; one that runs out hands over to the next.
func (p *HLSPlan) farTrackWindow(ctx context.Context, n, ti int, segStart, segEnd int64, skip bool, lastMs, lastOff, stopOff int64) ([]segSample, int64, bool, error) {
	if p.networkSrc(ctx) {
		return nil, -1, false, nil // every probe is a round trip there: the walk goes on as it did
	}
	src, err := p.openSource(ctx)
	if err != nil {
		return nil, -1, false, err
	}
	defer src.Close()
	p.winMu.Lock()
	p.stats.FarLookups++
	p.winMu.Unlock()
	var out []segSample
	regions := p.uncuedRegionsOf(src)
	from := 0 // the last region opening at or before the window, when they run in time order; the guard catches the rest
	for i := range regions {
		if p.regionFirstMs(src, i) <= segStart {
			from = i
		}
	}
	span := p.windowSpan(n)
	for i := from; i < len(regions); i++ {
		rg := regions[i]
		if rg.complete && rg.maxMs < segStart {
			continue // walked to its end once: it holds nothing as late as the window
		}
		if rg.start > stopOff && rg.start < stopOff+span {
			return nil, -1, false, farNearError{at: rg.start} // what lies between may be the track's: the walk reads up to the region, then looks again
		}
		got, next, state, err := p.farTrackIn(ctx, src, i, rg, n, ti, segStart, segEnd, skip, lastMs, lastOff)
		if err != nil {
			return nil, -1, false, err
		}
		switch state {
		case farCrossed:
			p.noteFarHit()
			return append(out, got...), next, true, nil
		case farElsewhere:
			return nil, -1, false, nil
		}
		out = append(out, got...)
		if ms := lastWalkedMs(got); ms >= 0 {
			lastMs = ms
		}
		lastOff = maxInt64(lastOff, lastWalkedOff(got))
		if len(out) == 0 || rg.end < 0 {
			continue
		}
		// The window's samples started in this region, which ends at a cued cluster: the track may go on there,
		// interleaved (an interleaved end after a block-ordered stretch), or in the next region (block by block).
		got, next, state, err = p.farTrackIn(ctx, src, -1, tailRegion{start: rg.end, end: -1, firstMs: -1, maxMs: -1}, n, ti, segStart, segEnd, skip, lastMs, lastOff)
		if err != nil {
			return nil, -1, false, err
		}
		if state != farElsewhere {
			p.noteFarHit()
			return append(out, got...), next, true, nil
		}
	}
	if len(out) > 0 {
		p.noteFarHit()
		return out, -1, true, nil // the track's content ends in the last region: this is its last window
	}
	return nil, -1, false, nil
}

// noteFarHit counts a window's track found in the uncued regions.
func (p *HLSPlan) noteFarHit() {
	p.winMu.Lock()
	p.stats.FarHits++
	p.winMu.Unlock()
}

// farTrackIn is farTrackWindow's walk of one uncued region (the i-th, which learns how far in time it runs when walked
// to its end); i -1 walks the cued clusters past a region, and gives up unless the first of them holds the track.
func (p *HLSPlan) farTrackIn(ctx context.Context, src io.ReadSeeker, i int, rg tailRegion, n, ti int, segStart, segEnd int64, skip bool, lastMs, lastOff int64) ([]segSample, int64, farState, error) {
	seat := p.tailSeat(src, rg, n, segStart)
	var br *reader.BlockReader
	defer p.countWalk(&br)()
	rs := src
	if rg.end >= 0 {
		rs = &boundedReadSeeker{r: src, limit: rg.end} // the region ends at the next cued cluster, whatever the reader keeps
	}
	br, err := p.blockReaderAt(rs, seat)
	if err != nil {
		return nil, -1, farElsewhere, err
	}
	p.tolerateDamage(br)
	pt := p.tracks[ti]
	br.SetTrackDefaultDurations(p.trackDurs)
	br.KeepTracks(pt.ft.outTrack.mkv.ID)
	br.SetHeaderOnly(skip)
	absentPast := int64(-1) // past this, no block of the track since the seat: it is not in this region here
	if p.medianSpan > 0 {
		absentPast = seat + 2*p.medianSpan
		if rg.end >= 0 && rg.end-seat <= 4*p.medianSpan {
			absentPast = rg.end // what is left of the region is short: walk it whole, the track may sit at its very end
		}
	}
	var out []segSample
	var peek nextPtsPeek
	first, crossed, maxMs := true, false, int64(-1)
	ended := func() ([]segSample, int64, farState, error) {
		p.segMu.Lock()
		if i >= 0 && i < len(p.uncuedRegions) {
			p.uncuedRegions[i].complete = true
			p.uncuedRegions[i].maxMs = maxInt64(p.uncuedRegions[i].maxMs, maxMs)
		}
		p.segMu.Unlock()
		return out, -1, farEnded, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, -1, farElsewhere, err
		}
		b, err := br.Next()
		if p.walkEnd(err) {
			return ended()
		}
		if err != nil {
			return nil, -1, farElsewhere, errf("read block: %w", err)
		}
		pos := br.Pos()
		if rg.end >= 0 && pos.ClusterStart >= rg.end {
			return ended()
		}
		if first {
			first = false
			if absentPast > 0 && pos.Off > absentPast {
				return nil, -1, farElsewhere, nil // nothing of the track for two windows past the seat: not stored here
			}
			if i < 0 && pos.ClusterStart != rg.start {
				return nil, -1, farElsewhere, nil // the cued cluster past the region holds none of the track: it goes on elsewhere
			}
			if t := b.BlockTimecode; t > segStart && (lastMs < segStart || t-lastMs > farTailGapMs) {
				return nil, -1, farElsewhere, nil
			}
		}
		maxMs = maxInt64(maxMs, b.BlockTimecode)
		if ms, ok := p.clusterMs(pos.ClusterTS); ok {
			p.markTail(pos.ClusterStart, ms)
		}
		if crossed {
			if peek.observe(b.Timecode, b.Keyframe) {
				break
			}
			continue
		}
		if b.BlockTimecode < segStart || (b.BlockTimecode <= lastMs && b.DataOffset <= lastOff) {
			continue // before the window, or a block the walk already holds (a later block may carry an earlier time: reordered video)
		}
		if b.BlockTimecode >= segEnd {
			crossed = true
			peek = startPeek(b.Timecode, pt.ft.outTrack.spec.video)
			p.learnSegPos(n+1, pos) // the next window opens here at the latest: a linear walk of it must not start past this block
			p.learnTrackPos(n+1, ti, pos)
			if !peek.pending() {
				break
			}
			br.SetHeaderOnly(true)
			continue
		}
		s, err := sampleOf(pt, b, skip)
		if err != nil {
			return nil, -1, farElsewhere, err
		}
		out = append(out, s)
	}
	return out, peek.min, farCrossed, nil
}

// learnTrackPos records where track ti's window n opens. Like learnSegPos,
// only the earliest block is kept and writes are idempotent.
func (p *HLSPlan) learnTrackPos(n, ti int, at reader.BlockPos) {
	if !at.Valid() || n < 0 || n >= p.segCount {
		return
	}
	p.segMu.Lock()
	defer p.segMu.Unlock()
	for len(p.trackPos) <= n {
		p.trackPos = append(p.trackPos, nil)
	}
	if p.trackPos[n] == nil {
		p.trackPos[n] = make([]reader.BlockPos, len(p.tracks))
	}
	if cur := p.trackPos[n][ti]; cur.Valid() && cur.Off <= at.Off {
		return
	}
	p.trackPos[n][ti] = at
}
