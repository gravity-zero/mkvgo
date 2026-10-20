package mp4

import (
	"math"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// subIndex serves a cursor's windows without a pass over all its cues: each cue's end is settled once, when its next
// later cue arrives (subtitle.SettleCueEnds' rule), and while the cues start in time order a window is found by
// search, the rare cue stating a span longer than subFastMaxCueDurMs kept aside.
type subIndex struct {
	ends      []int64 // per cue: its end, unsettledEnd while it waits on a later cue
	pending   []int   // the cues waiting, their starts not increasing: a later cue settles those it starts after
	long      []int   // cues stating a span longer than subFastMaxCueDurMs
	unordered bool    // a cue started before the one held before it: windows take every cue, as before the index
}

const unsettledEnd = math.MinInt64

// add appends cues to cur and to its index.
func (cur *subCursor) add(cues ...subtitle.Cue) {
	ix := &cur.index
	for _, c := range cues {
		i := len(cur.cues)
		if i > 0 && c.StartMs < cur.cues[i-1].StartMs {
			ix.unordered = true
		}
		cur.cues = append(cur.cues, c)
		for n := len(ix.pending); n > 0 && cur.cues[ix.pending[n-1]].StartMs < c.StartMs; n-- {
			k := ix.pending[n-1]
			ix.ends[k] = subtitle.CueEndMs(cur.cues[k].StartMs, cur.cues[k].Text, c.StartMs, true, 0)
			ix.pending = ix.pending[:n-1]
		}
		if c.EndMs > c.StartMs {
			ix.ends = append(ix.ends, c.EndMs)
			if c.EndMs-c.StartMs > subFastMaxCueDurMs {
				ix.long = append(ix.long, i)
			}
		} else {
			ix.ends = append(ix.ends, unsettledEnd)
			ix.pending = append(ix.pending, i)
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

// windowCues returns, in block order and with their ends settled, every held cue that can meet [from, to): while the
// cues start in time order, those starting within subFastMaxCueDurMs before from up to to and the long ones before,
// at the cost of the window and not of the cues before it; otherwise every cue.
func (cur *subCursor) windowCues(from, to int64) []subtitle.Cue {
	ix := &cur.index
	var pick []int
	if ix.unordered {
		pick = make([]int, len(cur.cues))
		for i := range pick {
			pick[i] = i
		}
	} else {
		lo := from
		if lo > math.MinInt64+subFastMaxCueDurMs {
			lo -= subFastMaxCueDurMs
		}
		a := sort.Search(len(cur.cues), func(k int) bool { return cur.cues[k].StartMs >= lo })
		b := max(a, sort.Search(len(cur.cues), func(k int) bool { return cur.cues[k].StartMs >= to }))
		for _, i := range ix.long {
			if i < a {
				pick = append(pick, i)
			}
		}
		for i := a; i < b; i++ {
			pick = append(pick, i)
		}
	}
	out := make([]subtitle.Cue, len(pick))
	for k, i := range pick {
		out[k] = cur.cues[i]
		out[k].EndMs = cur.endOf(i)
	}
	return out
}
