package mp4

import (
	"errors"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// damagedRange is a region of the source a window walk found unreadable: [start, end) was passed over, or the walk
// gave it up (refused) because no Cluster lay within damageCap of start.
type damagedRange struct {
	start, end int64
	refused    bool
}

// DefaultDamageBytes is how far past an unreadable element a window walk looks for the next Cluster when
// Options.DamageBytes is 0.
const DefaultDamageBytes = 64 << 20

// damageCap is Options.DamageBytes, or DefaultDamageBytes when it is unset.
func (p *HLSPlan) damageCap() int64 {
	if p.opts.DamageBytes > 0 {
		return p.opts.DamageBytes
	}
	return DefaultDamageBytes
}

// tolerateDamage makes a window walk pass over a damaged region under damageCap instead of failing on it, unless a
// negative Options.DamageBytes refuses any damage.
func (p *HLSPlan) tolerateDamage(br *reader.BlockReader) {
	if p.opts.DamageBytes < 0 {
		return
	}
	br.SetSkipDamage(true)
	br.SetDamageScan(p.damageCap())
}

// walkEnd is isBlockWalkEnd, save that under a negative Options.DamageBytes a damaged region is refused, not an end.
func (p *HLSPlan) walkEnd(err error) bool {
	if p.opts.DamageBytes < 0 && errors.Is(err, reader.ErrDamagedRegion) {
		return false
	}
	return isBlockWalkEnd(err)
}

// noteSkipped records the damaged regions a walk passed over, so the windows seated inside them start past them.
func (p *HLSPlan) noteSkipped(br *reader.BlockReader) {
	for _, r := range br.SkippedRanges() {
		p.noteDamage(damagedRange{start: r.From, end: r.To})
	}
}

// noteDamage records one damaged region, once, and counts it.
func (p *HLSPlan) noteDamage(d damagedRange) {
	p.segMu.Lock()
	defer p.segMu.Unlock()
	i := sort.Search(len(p.damaged), func(k int) bool { return p.damaged[k].start >= d.start })
	if i < len(p.damaged) && p.damaged[i].start == d.start {
		return
	}
	p.damaged = append(p.damaged, damagedRange{})
	copy(p.damaged[i+1:], p.damaged[i:])
	p.damaged[i] = d
	p.winMu.Lock()
	if d.refused {
		p.stats.DamageRefusals++
	} else {
		p.stats.DamageSkips++
	}
	p.winMu.Unlock()
}

// pastDamage returns where a walk seated at off starts: past the damaged region holding off, when one is known,
// and an error when that region was refused.
func (p *HLSPlan) pastDamage(off int64) (int64, error) {
	p.segMu.Lock()
	defer p.segMu.Unlock()
	i := sort.Search(len(p.damaged), func(k int) bool { return p.damaged[k].end > off })
	if i >= len(p.damaged) || p.damaged[i].start > off {
		return off, nil
	}
	d := p.damaged[i]
	if d.refused {
		return off, errf("read block: %w", &reader.DamageError{Offset: d.start, Resume: -1, Cause: errDamageKnown})
	}
	return d.end, nil
}

// seatedPastDamage reports whether window n's own cluster lies in a damaged region passed over: the walk opens past
// it, mid-GOP, on frames whose references were lost.
func (p *HLSPlan) seatedPastDamage(n int) bool {
	off, err := p.pastDamage(p.offsets[n])
	return err == nil && off != p.offsets[n]
}

// errDamageKnown is the cause of a refusal repeated from a region already given up, which is not counted again.
var errDamageKnown = errors.New("region already found unreadable")

// noteDamageRefused records a walk refused on a damaged region longer than damageCap, once per region.
func (p *HLSPlan) noteDamageRefused(err error) {
	var de *reader.DamageError
	if errors.As(err, &de) && de.Resume < 0 && !errors.Is(de.Cause, errDamageKnown) {
		p.noteDamage(damagedRange{start: de.Offset, end: de.Offset + p.damageCap(), refused: true})
	}
}
