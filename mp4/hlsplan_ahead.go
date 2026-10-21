package mp4

import (
	"context"
	"io"
	"math"
	"os"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// noteCluster records a cued cluster's offset and opening time, kept ascending and unique.
func (p *HLSPlan) noteCluster(off, ms int64) {
	if n := len(p.clusters); n == 0 || p.clusters[n-1] < off {
		p.clusters = append(p.clusters, off)
		p.clusterOpenMs = append(p.clusterOpenMs, ms)
	}
}

// learnAhead folds one non-video block in: how far its time runs past the
// start of the cued cluster after the one holding it, when it does.
func (p *HLSPlan) learnAhead(pos reader.BlockPos, b mkv.Block) {
	i := sort.Search(len(p.clusters), func(k int) bool { return p.clusters[k] > pos.ClusterStart })
	if i >= len(p.clusterOpenMs) {
		return
	}
	if adv := b.Timecode - p.clusterOpenMs[i]; adv > 0 {
		for {
			cur := p.aheadMs.Load()
			if adv <= cur || p.aheadMs.CompareAndSwap(cur, adv) {
				return
			}
		}
	}
}

// learnAheadHead walks the first cued cluster header-only once, so the
// advance a muxer gives its audio over its video is known before any cold
// window opens.
func (p *HLSPlan) learnAheadHead(ctx context.Context) error {
	if len(p.clusters) < 2 {
		return nil
	}
	return p.walkBlocksPos(ctx, p.clusters[0], p.clusters[1], true, func(b mkv.Block, pt *planTrack, pos reader.BlockPos) (bool, error) {
		if !pt.ft.outTrack.spec.video {
			p.learnAhead(pos, b)
		}
		return true, nil
	})
}

// hintWindow tells a source that can take it (a remote one) the bytes a walk
// of window n reads from start: up to the cluster that opens the next window,
// plus the look past it, fetched with one request.
func (p *HLSPlan) hintWindow(src io.Reader, n int, start int64) {
	end := int64(math.MaxInt64)
	if n+1 < len(p.offsets) {
		end = p.offsets[n+1] + windowPeekBytes
	}
	if f, ok := src.(*os.File); ok && end != math.MaxInt64 && !p.onNetwork(f) { // the walk reads through the window: have it all coming in at once
		adviseWillNeed(f, start, end-start)
	}
	if h, ok := src.(mkv.RangeHinter); ok {
		h.HintRange(start, end)
	}
}

// windowPeekBytes is how far past the next window's cluster a walk is
// expected to read: that cluster's opening blocks, peeked for the window's end.
const windowPeekBytes = 512 << 10

// coldStart is where a walk with nothing learned starts looking for the n-th
// window's blocks: the cued cluster before its boundary's (a muxer writes the
// audio of an instant a little ahead of its video, and a few milliseconds of
// advance already put a block of the window in the cluster before; measured on
// 2 % of a real library, never visible at the head of the file), further back
// when the source has shown a larger advance (aheadMs). The clusters before
// the boundary's are walked header-only (coldAhead); the window's first audio
// blocks can sit
// there (the full pass, in track order, keeps them; so must the plan).
func (p *HLSPlan) coldStart(n int) int64 {
	off := p.offsets[n]
	if n == 0 {
		return off
	}
	i := sort.Search(len(p.clusters), func(k int) bool { return p.clusters[k] >= off })
	if i == 0 || i >= len(p.clusters) || p.clusters[i] != off {
		return off
	}
	i--
	for ahead := p.aheadMs.Load(); i > 0 && p.clusterOpenMs[i] > p.bounds[n]-ahead; {
		i--
	}
	return p.clusters[i]
}

// aheadBlock is a block of window n stored before the window's own cluster, found by coldAhead.
type aheadBlock struct {
	b   mkv.Block
	pos reader.BlockPos
}

// coldAhead walks br, seated on the clusters before the window's own,
// header-only up to that cluster and returns, per track, the blocks already
// belonging to the window (timecode past segStart), with their payload fetched
// when the walk needs payloads. Everything else in those clusters costs its
// headers only. br is left for the caller to re-seat.
func (p *HLSPlan) coldAhead(ctx context.Context, src io.ReadSeeker, br *reader.BlockReader, n int, index map[uint64]int, segStart int64, structure, keepAudio bool) ([][]aheadBlock, error) {
	br.SetHeaderOnly(true)
	ahead := make([][]aheadBlock, len(p.tracks))
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, err := br.Next()
		if p.walkEnd(err) {
			break
		}
		if err != nil {
			return nil, errf("read block: %w", err)
		}
		pos := br.Pos()
		if pos.ClusterStart >= p.offsets[n] {
			break
		}
		ti, ok := index[b.TrackNumber]
		if !ok {
			continue
		}
		if !p.tracks[ti].ft.outTrack.spec.video {
			p.learnAhead(pos, b)
		}
		if b.BlockTimecode >= segStart {
			ahead[ti] = append(ahead[ti], aheadBlock{b: b, pos: pos})
		}
	}
	for ti := range ahead {
		pt := p.tracks[ti]
		if structure && (!keepAudio || pt.ft.outTrack.spec.video) {
			continue // the window records positions only
		}
		for k := range ahead[ti] {
			b := &ahead[ti][k].b
			data := make([]byte, b.Size)
			if _, err := src.Seek(b.DataOffset, io.SeekStart); err != nil {
				return nil, err
			}
			if _, err := io.ReadFull(src, data); err != nil {
				return nil, errf("read block ahead of its cluster: %w", err)
			}
			p.walkRead.Add(int64(len(data)))
			b.Data = data
		}
	}
	return ahead, nil
}

// segPosAt returns the known opening block of the n-th window, if a walk has
// revealed it (zero BlockPos otherwise: walk from the segment's cluster).
func (p *HLSPlan) segPosAt(n int) reader.BlockPos {
	if n < 0 {
		return reader.BlockPos{}
	}
	p.segMu.Lock()
	defer p.segMu.Unlock()
	if n >= len(p.segPos) {
		return reader.BlockPos{}
	}
	return p.segPos[n]
}

// learnSegPos records where a window opens. Only the FIRST block past the
// boundary is kept: a later track crossing the same boundary must not push the
// mark forward, or a resumed walk would open past a track's own first sample.
// The store grows on demand (a growing plan appends segments as the source is
// written). Writes are idempotent - two walks reaching the same boundary find
// the same block - so concurrent requests race harmlessly.
func (p *HLSPlan) learnSegPos(n int, at reader.BlockPos) {
	if !at.Valid() || n < 0 || n >= p.segCount {
		return
	}
	p.segMu.Lock()
	defer p.segMu.Unlock()
	for len(p.segPos) <= n {
		p.segPos = append(p.segPos, reader.BlockPos{})
	}
	if cur := p.segPos[n]; cur.Valid() && cur.Off <= at.Off {
		return // an earlier block already opens this window
	}
	p.segPos[n] = at
}
