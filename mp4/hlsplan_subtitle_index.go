package mp4

import (
	"math"
	"slices"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// subIndex serves a cursor's windows without a pass over all its cues: each cue's end is settled once, when its next
// later cue arrives (subtitle.SettleCueEnds' rule), and a window is found by search. A cue starting at or after every
// cue before it joins ordered, whose starts never decrease; one starting earlier joins late, a tree by start; the rare
// cue stating a span longer than subFastMaxCueDurMs is also kept in long, a tree by start that knows each subtree's
// latest end.
type subIndex struct {
	ends     []int64 // per cue: its end, unsettledEnd while it waits on a later cue
	pending  []int   // the cues waiting, their starts not increasing: a later cue settles those it starts after
	ordered  []int   // the cues in time order, by block order
	late     cueTree // the cues starting before one held before them
	long     cueTree // cues stating a span longer than subFastMaxCueDurMs
	maxStart int64   // the latest start held
}

const unsettledEnd = math.MinInt64

// add appends cues to cur and to its index.
func (cur *subCursor) add(cues ...subtitle.Cue) {
	ix := &cur.index
	for _, c := range cues {
		i := len(cur.cues)
		cur.cues = append(cur.cues, c)
		for n := len(ix.pending); n > 0 && cur.cues[ix.pending[n-1]].StartMs < c.StartMs; n-- {
			k := ix.pending[n-1]
			ix.ends[k] = subtitle.CueEndMs(cur.cues[k].StartMs, cur.cues[k].Text, c.StartMs, true, 0)
			ix.pending = ix.pending[:n-1]
		}
		if c.EndMs > c.StartMs {
			ix.ends = append(ix.ends, c.EndMs)
			if c.EndMs-c.StartMs > subFastMaxCueDurMs {
				ix.long.insert(c.StartMs, c.EndMs, i)
			}
		} else {
			ix.ends = append(ix.ends, unsettledEnd)
			ix.pending = append(ix.pending, i)
		}
		if i == 0 || c.StartMs >= ix.maxStart {
			ix.ordered = append(ix.ordered, i)
			ix.maxStart = c.StartMs
		} else {
			ix.late.insert(c.StartMs, math.MaxInt64, i)
		}
	}
}

// endOf is cue i's end as subtitle.SettleCueEnds gives it over the cues held: a cue still waiting ends on its reading.
func (cur *subCursor) endOf(i int) int64 {
	if e := cur.index.ends[i]; e != unsettledEnd {
		return e
	}
	return subtitle.CueEndMs(cur.cues[i].StartMs, cur.cues[i].Text, 0, false, 0)
}

// windowCues returns, in block order and with their ends settled, every held cue that can meet [from, to): those
// starting within subFastMaxCueDurMs before from up to to, and the long ones starting earlier, at the cost of the
// window and not of the cues before it.
func (cur *subCursor) windowCues(from, to int64) []subtitle.Cue {
	ix := &cur.index
	lo := from
	if lo > math.MinInt64+subFastMaxCueDurMs {
		lo -= subFastMaxCueDurMs
	}
	startOf := func(k int) int64 { return cur.cues[ix.ordered[k]].StartMs }
	a := sort.Search(len(ix.ordered), func(k int) bool { return startOf(k) >= lo })
	b := max(a, sort.Search(len(ix.ordered), func(k int) bool { return startOf(k) >= to }))
	pick := append([]int(nil), ix.ordered[a:b]...)
	ix.late.each(lo, to, math.MinInt64, func(i int) { pick = append(pick, i) })
	ix.long.each(math.MinInt64, lo, from, func(i int) { pick = append(pick, i) })
	slices.Sort(pick)
	out := make([]subtitle.Cue, len(pick))
	for k, i := range pick {
		out[k] = cur.cues[i]
		out[k].EndMs = cur.endOf(i)
	}
	return out
}

// cueTree is a treap of cue indices keyed by (start, index), each node knowing its subtree's latest end: inserting a
// cue costs O(log n), and visiting those starting in a range and ending after a time costs the visit, O(log n) a cue
// found, whatever order the cues come in.
type cueTree struct {
	nodes []cueNode
	root  int32 // -1 when empty, as the zero value holds once insert ran
	seed  uint32
	used  bool
}

type cueNode struct {
	start, end, maxEnd int64
	idx                int32
	pri                uint32
	left, right        int32
}

// pull sets n's maxEnd from its own end and its children's.
func (t *cueTree) pull(n int32) {
	m := t.nodes[n].end
	for _, c := range [2]int32{t.nodes[n].left, t.nodes[n].right} {
		if c >= 0 {
			m = max(m, t.nodes[c].maxEnd)
		}
	}
	t.nodes[n].maxEnd = m
}

func (t *cueTree) less(a int32, start int64, idx int) bool {
	n := &t.nodes[a]
	return n.start < start || (n.start == start && int(n.idx) < idx)
}

// split cuts the tree at n into the nodes before (start, idx) and those at or after it.
func (t *cueTree) split(n int32, start int64, idx int) (int32, int32) {
	if n < 0 {
		return -1, -1
	}
	if t.less(n, start, idx) {
		l, r := t.split(t.nodes[n].right, start, idx)
		t.nodes[n].right = l
		t.pull(n)
		return n, r
	}
	l, r := t.split(t.nodes[n].left, start, idx)
	t.nodes[n].left = r
	t.pull(n)
	return l, n
}

// join links two trees whose keys are all ordered, a's before b's.
func (t *cueTree) join(a, b int32) int32 {
	switch {
	case a < 0:
		return b
	case b < 0:
		return a
	case t.nodes[a].pri > t.nodes[b].pri:
		t.nodes[a].right = t.join(t.nodes[a].right, b)
		t.pull(a)
		return a
	default:
		t.nodes[b].left = t.join(a, t.nodes[b].left)
		t.pull(b)
		return b
	}
}

func (t *cueTree) insert(start, end int64, idx int) {
	if !t.used {
		t.root, t.seed, t.used = -1, 2463534242, true
	}
	t.seed ^= t.seed << 13
	t.seed ^= t.seed >> 17
	t.seed ^= t.seed << 5
	n := int32(len(t.nodes))
	t.nodes = append(t.nodes, cueNode{start: start, end: end, maxEnd: end, idx: int32(idx), pri: t.seed, left: -1, right: -1})
	l, r := t.split(t.root, start, idx)
	t.root = t.join(t.join(l, n), r)
}

// each visits, in no particular order, every cue index starting in [lo, to) and ending after after; a subtree ending
// by then is not entered.
func (t *cueTree) each(lo, to, after int64, fn func(int)) {
	if !t.used {
		return
	}
	stack := []int32{t.root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n < 0 || t.nodes[n].maxEnd <= after {
			continue
		}
		s := t.nodes[n].start
		if s >= lo && s < to && t.nodes[n].end > after {
			fn(int(t.nodes[n].idx))
		}
		if s >= lo {
			stack = append(stack, t.nodes[n].left)
		}
		if s < to {
			stack = append(stack, t.nodes[n].right)
		}
	}
}
