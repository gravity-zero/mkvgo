package ops

import (
	"io"
	"slices"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// subtitleDamage passes a subtitle read over the damaged regions Options.DamageBytes allows and hands each to OnSkip.
type subtitleDamage struct {
	br     *reader.BlockReader
	opts   []mkv.Options
	told   int
	lastTo int64 // where the last region reported ends: entries of one region read from an index report it once
}

// tolerateSubtitleDamage makes br skip a damaged region under the bound opts give; a read refusing damage keeps br strict.
func tolerateSubtitleDamage(br *reader.BlockReader, opts []mkv.Options) *subtitleDamage {
	if n := mkv.DamageBytesFrom(opts); n > 0 {
		br.SetSkipDamage(true)
		br.SetDamageScan(n)
	}
	return &subtitleDamage{br: br, opts: opts}
}

// pending reports whether the reader passed over a region the last skipped call did not report.
func (d *subtitleDamage) pending() bool { return len(d.br.SkippedRanges()) > d.told }

// skipped reports the regions passed over since the last call and returns the earliest one's start time (ms), -1 for none.
func (d *subtitleDamage) skipped() (fromMs int64, ok bool) {
	r := d.br.SkippedRanges()
	fromMs = -1
	for ; d.told < len(r); d.told++ {
		g := r[d.told]
		if !ok {
			fromMs, ok = g.FromMs, true
		}
		if g.To != d.lastTo {
			reportDamage(d.opts, g, g.FromMs)
		}
		d.lastTo = g.To
	}
	return fromMs, ok
}

// seatZeroed reports whether the bytes an index entry seats on are null: damage, where other bytes mean a stale index.
func seatZeroed(f io.ReadSeeker, off int64) bool {
	var b [16]byte
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return false
	}
	n, _ := io.ReadFull(f, b[:])
	return n > 0 && !slices.ContainsFunc(b[:n], func(c byte) bool { return c != 0 })
}

// damageAtCluster reports the damaged region a cued cluster that does not resolve starts, when opts let a read pass over it.
func damageAtCluster(f io.ReadSeeker, tcScale, at int64, opts []mkv.Options) (reader.DamageRange, bool) {
	n := mkv.DamageBytesFrom(opts)
	if n <= 0 || !seatZeroed(f, at) {
		return reader.DamageRange{}, false
	}
	br, err := reader.NewBlockReaderAt(f, tcScale, at)
	if err != nil {
		return reader.DamageRange{}, false
	}
	br.SetSkipDamage(true)
	br.SetDamageScan(n)
	br.SetHeaderOnly(true)
	_, _ = br.Next()
	r := br.SkippedRanges()
	if len(r) == 0 || r[0].From < at || r[0].From >= at+clusterHeadBytes {
		return reader.DamageRange{}, false
	}
	g := r[0]
	g.From = at // the cluster is lost from its own start
	return g, true
}

// clusterHeadBytes is how far into a cluster ResolveClusterBlock reads: its header and its Timestamp.
const clusterHeadBytes = 24

// inDamage reports whether off lies in one of the regions.
func inDamage(regions []reader.DamageRange, off int64) bool {
	for _, g := range regions {
		if off >= g.From && off < g.To {
			return true
		}
	}
	return false
}

// reportDamage hands OnSkip a region a read passed over, timed at atMs when the region's own start time is unknown.
func reportDamage(opts []mkv.Options, g reader.DamageRange, atMs int64) {
	onSkip := mkv.OnSkipFrom(opts)
	if onSkip == nil {
		return
	}
	from, end := g.FromMs, g.ToMs
	if from < 0 {
		from = max(atMs, 0) // before any Cluster: from the start
	}
	if end < 0 {
		end = from
	}
	onSkip(mkv.DamagedRange{StartOffset: g.From, EndOffset: g.To, ApproxStartMs: from, ApproxEndMs: end})
}

// closeOpenCue ends the last cue, when it has no end of its own, as subtitle.CueEndMs does with the region passed
// over standing for the next cue: the next real cue lies past the region and would stretch it over the lost span.
func closeOpenCue(cues []subtitle.Cue, fromMs int64, ok bool) {
	if !ok || len(cues) == 0 || cues[len(cues)-1].EndMs > cues[len(cues)-1].StartMs {
		return
	}
	c := &cues[len(cues)-1]
	c.EndMs = subtitle.CueEndMs(c.StartMs, c.Text, fromMs, true, 0)
}
