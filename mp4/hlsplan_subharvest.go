package mp4

import (
	"context"
	"math"
	"sort"
	"sync"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// subHarvest holds the subtitle blocks the window walks pass over. A window walk reads every block of the clusters
// it covers, the subtitle tracks' included, so a subtitle cursor takes its cues from there instead of walking those
// clusters a second time (a sequential viewer with one subtitle read the file twice, three subtitles four times).
// A cluster serves once the walks that met it read it whole between them; any other is walked as before.
type subHarvest struct {
	mu       sync.Mutex
	clusters map[int64]*harvestCluster // by the cluster's start offset
	held     int                       // blocks held, bounded by maxHarvestBlocks
	off      bool
}

// maxHarvestBlocks bounds the subtitle blocks a plan holds from its walks (a feature film's text tracks hold a few
// thousand); past it, clusters not yet met are walked as before.
const maxHarvestBlocks = 1 << 18

// harvestCluster is what the walks read of one cluster.
type harvestCluster struct {
	ms     int64      // the cluster's timestamp
	spans  [][2]int64 // block offsets read through, [lo, hi]; lo = the cluster's start when read from it, hi = MaxInt64 to its end
	next   int64      // the next cluster a walk met holding a kept block, -1 unknown, harvestEnd past the last
	blocks []harvestBlock
	whole  bool // spans cover the cluster and next is known
}

// harvestEnd is a cluster's next when a walk read it to the source's clean end.
const harvestEnd = -2

// harvestBlock is one subtitle frame as the cursor's walk would turn it into a cue.
type harvestBlock struct {
	off           int64
	frame         int // index within a laced block
	sub           int // index in HLSPlan.subs
	cue           subtitle.Cue
	ok, recovered bool
	err           error
}

// harvestRec records one window walk, committed when the walk ends without error.
type harvestRec struct {
	p      *HLSPlan
	subOf  map[uint64]int
	seat   int64 // offset the reader was seated at
	parts  []*harvestCluster
	starts []int64
	broken bool
}

// harvestTracks returns the subtitle track IDs a window walk should read along, nil when none.
func (p *HLSPlan) harvestTracks() map[uint64]int {
	if p.mp4src || len(p.subs) == 0 || p.harvest.off {
		return nil
	}
	ids := make(map[uint64]int, len(p.subs))
	for i := range p.subs {
		ids[p.subs[i].track.ID] = i
	}
	return ids
}

// newHarvestRec starts a recording for a walk seated at seat; nil when the plan harvests nothing.
func (p *HLSPlan) newHarvestRec(subOf map[uint64]int, seat int64) *harvestRec {
	if subOf == nil {
		return nil
	}
	return &harvestRec{p: p, subOf: subOf, seat: seat}
}

// observe folds in the block the walk's reader just returned, sub being its index in p.subs or -1 for a media block.
func (r *harvestRec) observe(pos reader.BlockPos, b *mkv.Block, sub int) {
	if r == nil || r.broken {
		return
	}
	n := len(r.parts)
	if n == 0 || r.starts[n-1] != pos.ClusterStart {
		ms, ok := r.p.clusterMs(pos.ClusterTS)
		if !ok || (n > 0 && pos.ClusterStart < r.starts[n-1]) {
			r.broken = true
			return
		}
		lo := pos.ClusterStart
		if n > 0 {
			prev := r.parts[n-1]
			prev.spans[0][1] = math.MaxInt64 // read to its end: the reader went on to this cluster
			prev.next = pos.ClusterStart
		} else if r.seat > pos.ClusterStart {
			lo = r.seat // seated inside the cluster: what precedes the seat was not read
		}
		r.parts = append(r.parts, &harvestCluster{ms: ms, spans: [][2]int64{{lo, pos.Off}}, next: -1})
		r.starts = append(r.starts, pos.ClusterStart)
		n++
	}
	c := r.parts[n-1]
	if pos.Off > c.spans[0][1] {
		c.spans[0][1] = pos.Off
	}
	if sub < 0 {
		return
	}
	if b.Data == nil && b.Size > 0 { // a payload the reader skipped: no cue to take
		r.broken = true
		return
	}
	frame := 0
	if k := len(c.blocks); k > 0 && c.blocks[k-1].off == pos.Off {
		frame = c.blocks[k-1].frame + 1
	}
	cue, ok, recovered, err := subCueFromBlockRecovered(&r.p.subs[sub].track, *b)
	c.blocks = append(c.blocks, harvestBlock{off: pos.Off, frame: frame, sub: sub, cue: cue, ok: ok, recovered: recovered, err: err})
}

// end marks the walk's last cluster read to the source's clean end.
func (r *harvestRec) end() {
	if r == nil || r.broken || len(r.parts) == 0 {
		return
	}
	last := r.parts[len(r.parts)-1]
	last.spans[0][1], last.next = math.MaxInt64, harvestEnd
}

// commit merges a finished walk's clusters into the plan's harvest.
func (r *harvestRec) commit() {
	if r == nil || r.broken || len(r.parts) == 0 {
		return
	}
	h := &r.p.harvest
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clusters == nil {
		h.clusters = make(map[int64]*harvestCluster)
	}
	for k, part := range r.parts {
		c := h.clusters[r.starts[k]]
		if c == nil {
			if h.held+len(part.blocks) > maxHarvestBlocks {
				continue
			}
			c = &harvestCluster{ms: part.ms, next: -1}
			h.clusters[r.starts[k]] = c
		}
		if c.whole {
			continue
		}
		c.spans = append(c.spans, part.spans[0])
		if part.next != -1 {
			c.next = part.next
		}
		before := len(c.blocks)
		c.blocks = mergeHarvested(c.blocks, part.blocks)
		h.held += len(c.blocks) - before
		c.whole = c.next != -1 && spansCover(c.spans, r.starts[k])
	}
}

// mergeHarvested adds the blocks of b that a is missing, keeping block order.
func mergeHarvested(a, b []harvestBlock) []harvestBlock {
	if len(a) == 0 {
		return append([]harvestBlock(nil), b...)
	}
	type key struct {
		off   int64
		frame int
	}
	have := make(map[key]bool, len(a))
	for _, x := range a {
		have[key{x.off, x.frame}] = true
	}
	added := false
	for _, x := range b {
		if !have[key{x.off, x.frame}] {
			a, added = append(a, x), true
		}
	}
	if added {
		sort.SliceStable(a, func(i, j int) bool {
			if a[i].off != a[j].off {
				return a[i].off < a[j].off
			}
			return a[i].frame < a[j].frame
		})
	}
	return a
}

// spansCover reports whether the spans read a cluster starting at start through to its end with no gap.
func spansCover(spans [][2]int64, start int64) bool {
	s := append([][2]int64(nil), spans...)
	sort.Slice(s, func(i, j int) bool { return s[i][0] < s[j][0] })
	if s[0][0] > start {
		return false
	}
	reach := s[0][1]
	for _, x := range s[1:] {
		if x[0] > reach {
			return false
		}
		reach = max(reach, x[1])
	}
	return reach == math.MaxInt64
}

// harvested returns the cluster at start as the walks read it: its timestamp (known), and when read whole, the
// subtitle track's blocks in it and the next cluster's start.
func (h *subHarvest) harvested(start int64, sub int) (ms int64, known, whole bool, next int64, blocks []harvestBlock) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.clusters[start]
	if c == nil {
		return 0, false, false, -1, nil
	}
	if !c.whole {
		return c.ms, true, false, -1, nil
	}
	for _, b := range c.blocks {
		if b.sub == sub {
			blocks = append(blocks, b)
		}
	}
	return c.ms, true, true, c.next, blocks
}

// prebuildsTables reports whether a subtitle cursor may build the window tables it needs: a local Matroska source
// the segments are written from, whose tables are structure only, so the video then serves from the very walk.
func (p *HLSPlan) prebuildsTables(ctx context.Context) bool {
	return !p.mp4src && p.streams() && !p.networkSrc(ctx)
}

// harvestWindow builds the table of a window whose walk reads the cluster at off, one not built yet in this scan;
// it reports whether it built one.
func (p *HLSPlan) harvestWindow(ctx context.Context, off int64, tried map[int]bool) bool {
	k := sort.Search(min(len(p.offsets), p.segCount), func(k int) bool { return p.offsets[k] > off }) - 1
	for _, n := range []int{k, k - 1} { // a walk seated past the cluster's start leaves its head to the previous window's
		if n < 0 || tried[n] {
			continue
		}
		tried[n] = true
		if _, err := p.table(ctx, n); err != nil {
			return false
		}
		return true
	}
	return false
}

// extendCursorHarvested is extendCursorLocked's scan served from the clusters the window walks read: stride by
// stride, the same blocks in the same order, building the window tables it lacks when the plan writes its segments
// from them. served is false when a cluster it needs was not read whole: the cursor stands on that cluster with
// what precedes it committed, and the walk serves from there. Caller holds subMu[i].
func (p *HLSPlan) extendCursorHarvested(ctx context.Context, i int, cur *subCursor, target, uptoMs int64) (served bool, err error) {
	st := &p.subScan[i]
	build := p.prebuildsTables(ctx)
	tried := map[int]bool{}
	var took int64
	defer func() {
		p.winMu.Lock()
		p.stats.SubtitleHarvestedClusters += took
		p.winMu.Unlock()
	}()
	for {
		stopAt := cur.scanned + subScanStrideMs
		if stopAt < cur.scanned {
			stopAt = math.MaxInt64
		}
		if target > cur.scanned && target < stopAt {
			stopAt = target // a window built ahead is a walk the video may never use: build no further than this request needs
		}
		next := cur.next
		if next == 0 {
			next = p.offsets[0]
		}
		var local []subtitle.Cue
		for {
			ms, known, whole, after, blocks := p.harvest.harvested(next, i)
			if known && ms > stopAt {
				break // the walk's StopBeforeClusterMs: the stride ends before this cluster
			}
			if !whole {
				if build && p.harvestWindow(ctx, next, tried) {
					continue
				}
				cur.add(local...)
				cur.next = next
				if err := ctx.Err(); err != nil {
					return true, err
				}
				return false, nil
			}
			for _, b := range blocks {
				if b.err != nil {
					st.err = b.err // the cue's own bytes: every scan would refuse them again
					return true, st.err
				}
				p.noteRecovered(b.recovered)
				if b.ok {
					if b.cue.EndMs <= b.cue.StartMs || b.cue.EndMs-b.cue.StartMs > subFastMaxCueDurMs {
						st.noFast = true
					}
					local = append(local, b.cue)
				}
			}
			took++
			if after == harvestEnd {
				cur.add(local...)
				cur.done = true
				return true, nil
			}
			next = after
		}
		cur.add(local...)
		cur.next, cur.scanned = next, stopAt
		if st.noFast && cur.baseMs > 0 {
			return true, nil // the island is doomed; the caller falls back to the prefix
		}
		if cur.scanned >= target && !subNeedsNextCue(cur.cues, uptoMs) {
			return true, nil
		}
		if err := ctx.Err(); err != nil {
			return true, err
		}
	}
}
