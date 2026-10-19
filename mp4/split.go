package mp4

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/internal/splitplan"
	"github.com/gravity-zero/mkvgo/mkv"
)

const nsPerSec = 1_000_000_000

// cutTrack is one probe track of a source being cut or joined.
type cutTrack struct {
	trak    []byte
	tt      *trakTable
	edit    simpleEdit
	ts      uint32
	handler string
	endNs   int64 // where its presentation ends on the movie timeline
}

// cutModel is a source's moov as split and join see it: its probe tracks expanded, the rest of the moov verbatim.
type cutModel struct {
	path    string
	movieTS uint32
	outTS   uint32 // the output's movie timescale: a common multiple of the movie's and the tracks', so cuts stay exact
	movieNs int64  // presentation length
	tracks  []cutTrack
	others  []memBox // moov children other than traks
	meta    *mkv.Container
	ref     int   // the track cuts land on: the first video track, else the first
	held    int64 // bytes of moov the model keeps
	link    splitLink
	seed    [32]byte // the moov's hash, the parts' links derive from it
}

// loadCutModel opens a source's moov into a cutModel, chunk offsets shifted by base, samples taken from budget.
func loadCutModel(r io.ReadSeeker, path string, size, base int64, budget *int64, o Options) (*cutModel, error) {
	payload, err := readMoovLimit(r, size, o.moovLimit())
	if err != nil {
		return nil, err
	}
	mdats := sourceMdats(r, size)
	meta, mv, err := moovContainer(r, payload, size)
	if err != nil {
		return nil, err
	}
	if mv.fragmented {
		return nil, fmt.Errorf("a fragmented MP4 is not cut sample by sample; remux it to a progressive MP4 first (from-mp4, then to-mp4)")
	}
	for _, d := range mv.dropped {
		d.Reason += " (not carried)"
		o.report(d)
	}
	boxes, err := iterBoxes(payload)
	if err != nil {
		return nil, err
	}
	m := &cutModel{path: path, meta: meta, ref: -1, held: int64(len(payload)), link: readSplitLink(boxes), seed: sha256.Sum256(payload)}
	var traks [][]byte
	for _, b := range boxes {
		switch b.typ {
		case "trak":
			traks = append(traks, b.payload)
		case "mvhd":
			ts, dur := parseMovieHeader(b.payload)
			if ts == 0 {
				return nil, fmt.Errorf("mvhd declares no timescale")
			}
			m.movieTS = ts
			if headerDurationSet(dur) {
				m.movieNs = rescale(int64(dur), ts, nsPerSec) // unset: taken from the tracks below
			}
			m.others = append(m.others, b)
		default:
			m.others = append(m.others, b)
		}
	}
	if m.movieTS == 0 {
		return nil, fmt.Errorf("moov without mvhd")
	}
	for i := range mv.tracks {
		trak := traks[mv.tracks[i].trakIndex]
		if err := relocatable(trak); err != nil {
			return nil, fmt.Errorf("track %d: %w", i+1, err)
		}
		tb, err := iterBoxes(trak)
		if err != nil {
			return nil, err
		}
		edit, err := trakSimpleEdit(tb)
		if err != nil {
			return nil, fmt.Errorf("track %d: %w", i+1, err)
		}
		tt, err := readTrakTable(trak, base, base+size, budget, mdats)
		if err != nil {
			return nil, fmt.Errorf("track %d: %w", i+1, err)
		}
		mdhd, err := boxAt(trak, "mdia", "mdhd")
		if err != nil {
			return nil, fmt.Errorf("track %d: %w", i+1, err)
		}
		ts, _ := parseMdhd(mdhd)
		if ts == 0 {
			return nil, fmt.Errorf("track %d: mdhd declares no timescale", i+1)
		}
		ct := cutTrack{trak: trak, tt: tt, edit: edit, ts: ts, handler: trakHandler(tb)}
		if m.ref < 0 && mv.tracks[i].trackType == mkv.VideoTrack {
			m.ref = i
		}
		m.tracks = append(m.tracks, ct)
	}
	if len(m.tracks) == 0 {
		return nil, fmt.Errorf("no media track to cut")
	}
	if m.ref < 0 {
		m.ref = 0
	}
	keep := m.keptIDs()
	m.outTS = m.movieTS
	for i := range m.tracks {
		c := &m.tracks[i]
		c.endNs = c.presentEndNs(m.movieTS)
		m.outTS = commonTimescale(m.outTS, c.ts)
		if c.trak, err = stripTref(c.trak, keep); err != nil {
			return nil, err
		}
	}
	if m.movieNs == 0 {
		for i := range m.tracks {
			m.movieNs = max(m.movieNs, m.tracks[i].endNs)
		}
	}
	return m, nil
}

// ptNs is the presentation time of sample i on the source's movie timeline.
func (c *cutTrack) ptNs(i int, movieTS uint32) int64 {
	s := c.tt.samples[i]
	return rescale(c.edit.empty, movieTS, nsPerSec) + rescale(s.dts+int64(s.cto)-c.edit.mediaTime, c.ts, nsPerSec)
}

// presentEndNs is where the track's presentation ends on the movie timeline.
func (c *cutTrack) presentEndNs(movieTS uint32) int64 {
	if c.endNs != 0 {
		return c.endNs
	}
	if c.edit.segDur >= 0 {
		return rescale(c.edit.empty+c.edit.segDur, movieTS, nsPerSec)
	}
	return rescale(c.edit.empty, movieTS, nsPerSec) + rescale(c.tt.maxEnd()-c.edit.mediaTime, c.ts, nsPerSec)
}

// otherVideo reports whether track i is a video track other than the one cuts land on.
func (c *cutTrack) otherVideo(i, ref int) bool { return i != ref && c.handler == "vide" }

// isText reports whether the track is timed text, whose samples hold the screen until the next one.
func (c *cutTrack) isText() bool {
	switch c.handler {
	case "text", "sbtl", "subt":
		return true
	}
	return false
}

// firstSyncFrom is the first sync sample at or after decode index from whose presentation is at or past bound (len when none).
func (c *cutTrack) firstSyncFrom(from int, movieTS uint32, atOrPast func(ptNs int64) bool) int {
	for i := from; i < len(c.tt.samples); i++ {
		if c.tt.samples[i].sync && atOrPast(c.ptNs(i, movieTS)) {
			return i
		}
	}
	return len(c.tt.samples)
}

// keyframesMs lists the cut track's shown sync-sample presentation times, ascending.
func (m *cutModel) keyframesMs() []int64 {
	c := &m.tracks[m.ref]
	end := c.presentEndNs(m.movieTS)
	var out []int64
	for i, s := range c.tt.samples {
		if s.sync && c.ptNs(i, m.movieTS) < end {
			out = append(out, roundMs(c.ptNs(i, m.movieTS)))
		}
	}
	return sortDedupTimes(out)
}

func roundMs(ns int64) int64 {
	if ns < 0 {
		return -((-ns + 500_000) / 1_000_000)
	}
	return (ns + 500_000) / 1_000_000
}

// partEdit builds a cut track's edts (samples tt from source decode index a, window tsNs..teNs) and its tkhd duration.
func partEdit(c *cutTrack, tt *trakTable, a int, srcTS, outTS uint32, tsNs, teNs int64) ([]byte, int64) {
	if len(tt.samples) == 0 {
		return nil, 0
	}
	dtsA := c.tt.samples[a].dts
	// The part's media starts at its earliest sample, or later where the source's edit list hides some (priming).
	start := max(tt.minCts(), c.edit.mediaTime-dtsA)
	e0 := rescale(c.edit.empty, srcTS, nsPerSec) - tsNs + rescale(dtsA-c.edit.mediaTime+start, c.ts, nsPerSec)
	empty, mediaTime := int64(0), start
	if e0 >= 0 {
		empty = rescale(e0, nsPerSec, outTS)
	} else {
		mediaTime = start + rescale(-e0, nsPerSec, c.ts)
	}
	segDur := rescale(max(tt.maxEnd()-mediaTime, 0), c.ts, outTS)
	limitNs := c.presentEndNs(srcTS)
	if teNs > 0 {
		limitNs = min(limitNs, teNs)
	}
	if lim := rescale(limitNs-tsNs, nsPerSec, outTS) - empty; segDur > lim {
		segDur = max(lim, 0)
	}
	return buildEdit(empty, mediaTime, segDur), empty + segDur
}

// assemble builds a virtual moov payload: mvhd with the movie duration, then the given traks, then the other children.
func (m *cutModel) assemble(traks [][]byte, movieDur int64, movieTS uint32) ([]byte, error) {
	var w bw
	for _, b := range m.others {
		if b.typ != "mvhd" {
			continue
		}
		p, err := patchMvhdTimescale(b.payload, movieTS)
		if err != nil {
			return nil, err
		}
		if p, err = patchMvhdDuration(p, movieDur); err != nil {
			return nil, err
		}
		w.bytes(box("mvhd", p))
		for _, t := range traks {
			w.bytes(box("trak", t))
		}
	}
	for _, b := range m.others {
		if b.typ != "mvhd" {
			w.bytes(box(b.typ, b.payload))
		}
	}
	return w.b, nil
}

// withChapters rewrites a virtual moov payload's chapters to chs; the chapter samples join vs.
func withChapters(r io.ReadSeeker, payload []byte, vs vsource, chs []mkv.Chapter) ([]byte, vsource, error) {
	limit := vs.limit()
	before, mv, err := moovContainer(r, payload, limit)
	if err != nil {
		return nil, nil, err
	}
	after, _, err := moovContainer(r, payload, limit)
	if err != nil {
		return nil, nil, err
	}
	after.Chapters = chs
	if len(chs) > 0 {
		before.Chapters = nil // the source's chapter track is not carried: rebuild one even when the chapters come out unchanged
	}
	moov, chap, err := patchMoovMeta(payload, mv, before, after, limit)
	if err != nil {
		return nil, nil, err
	}
	mb, err := iterBoxes(moov)
	if err != nil || len(mb) != 1 {
		return nil, nil, fmt.Errorf("rebuilt moov does not parse")
	}
	return mb[0].payload, vs.withMem(chap), nil
}

// stripTref drops a trak's chapter references (the chapters are rebuilt) and its references to traks outside keep.
func stripTref(trak []byte, keep map[uint32]bool) ([]byte, error) {
	tb, err := iterBoxes(trak)
	if err != nil {
		return nil, err
	}
	tref, ok := findMemBox(tb, "tref")
	if !ok {
		return trak, nil
	}
	refs, err := iterBoxes(tref.payload)
	if err != nil {
		return nil, err
	}
	gone := map[uint32]bool{}
	for _, r := range refs {
		for off := 0; off+4 <= len(r.payload); off += 4 {
			if id := binary.BigEndian.Uint32(r.payload[off:]); !keep[id] || r.typ == "chap" {
				gone[id] = true
			}
		}
	}
	return dropTrefIDs(trak, gone)
}

// keptIDs is the set of the model's probe track IDs.
func (m *cutModel) keptIDs() map[uint32]bool {
	ids := map[uint32]bool{}
	for _, c := range m.tracks {
		if id, ok := trakID(c.trak); ok {
			ids[id] = true
		}
	}
	return ids
}

// Split cuts an MP4/MOV into parts opening on video keyframes, samples copied untouched; opts follow matroska.Split.
func Split(ctx context.Context, opts mkv.SplitOptions, extra ...Options) ([]string, error) {
	o := optionsFrom(extra)
	src, err := o.FS.DoOpen(opts.SourcePath)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	size, err := src.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	budget := int64(maxSamples)
	m, err := loadCutModel(src, opts.SourcePath, size, 0, &budget, o)
	if err != nil {
		return nil, errf("split: %s: %w", shown.Path(opts.SourcePath), err)
	}
	ftyp, err := sourceFtyp(src, size)
	if err != nil {
		return nil, err
	}
	top, err := sourceTopBoxes(src, size)
	if err != nil {
		return nil, err
	}
	lay := outLayout{ftyp: ftyp, top: top}
	var ranges []mkv.TimeRange
	var titles []string
	var chs []mkv.Chapter
	switch {
	case opts.ByChapters:
		if len(m.meta.Chapters) == 0 {
			return nil, errf("split: no chapters to split by")
		}
		lastKey := lastKeyMs(&m.tracks[m.ref], m.movieTS)
		for _, ch := range splitplan.ByStart(m.meta.Chapters) {
			if ch.StartMs > lastKey && len(chs) > 0 {
				continue // no keyframe left to open a part on: the previous part runs on through it
			}
			chs = append(chs, ch)
			titles = append(titles, ch.Title)
		}
		ranges = splitplan.ChapterRanges(chs)
		if n := len(ranges); n > 0 && len(chs) < len(m.meta.Chapters) {
			ranges[n-1].EndMs = 0
		}
	case opts.EveryMs > 0:
		if ranges, err = splitplan.EveryRanges(m.keyframesMs(), opts.EveryMs); err != nil {
			return nil, errf("split: %w", err)
		}
	default:
		ranges = opts.Ranges
		if err := splitplan.CheckRanges(ranges, m.meta.DurationMs); err != nil {
			return nil, errf("split: %w", err)
		}
	}
	if len(ranges) == 0 {
		return nil, errf("split: no split ranges specified")
	}
	pattern := opts.Pattern
	if pattern == "" {
		pattern = "part_%03d.mp4"
	}
	if err := splitplan.CheckPattern(pattern); err != nil {
		return nil, errf("split: %w", err)
	}
	// Every part is planned before the first is written: a range that cannot be cut leaves no part behind.
	used := map[string]bool{}
	plans := make([]partPlan, len(ranges))
	paths := make([]string, len(ranges))
	cur := make([]int, len(m.tracks))
	prevStart := int64(-1)
	for i, r := range ranges {
		if r.StartMs < prevStart {
			clear(cur)
		}
		prevStart = r.StartMs
		if plans[i], err = m.planPart(r, cur); err != nil {
			if nk := (*noKeyframeInRange)(nil); opts.ByChapters && errors.As(err, &nk) {
				return nil, errf("split: chapter %q (%d-%d ms) holds no keyframe (the next one is at %d ms): split by time instead (-range or -every, SplitOptions.Ranges or EveryMs), or move the chapter (set-chapters)", titleAt(titles, i), nk.startMs, nk.endMs, nk.nextMs)
			}
			return nil, errf("split: part %d: %w", i+1, err)
		}
		name := splitplan.PartName(pattern, i, titleAt(titles, i), i < len(titles), used)
		if paths[i], err = splitplan.SafePath(opts.OutputDir, name); err != nil {
			return nil, errf("split: %w", err)
		}
		if err := distinctOutput(o.FS, paths[i], opts.SourcePath); err != nil {
			return nil, err
		}
	}
	if err := o.FS.DoMkdirAll(opts.OutputDir, 0o755); err != nil {
		return nil, err
	}
	var outputs []string
	bounds := make([][2]int64, len(plans))
	for i, p := range plans {
		bounds[i] = [2]int64{p.tsNs, p.teNs}
	}
	links := splitLinks(m.seed, m.link, bounds)
	for i := range plans {
		if err := ctx.Err(); err != nil {
			return outputs, err
		}
		if err := m.writePart(ctx, src, size, lay, plans[i], links[i], paths[i], o); err != nil {
			return outputs, errf("split: part %d: %w", i+1, err)
		}
		outputs = append(outputs, paths[i])
	}
	return outputs, nil
}

// noKeyframeInRange is a range with no keyframe of its own; Split words the remedy for the way the ranges were given.
type noKeyframeInRange struct{ startMs, endMs, nextMs int64 }

func (e *noKeyframeInRange) Error() string {
	return fmt.Sprintf("the range %d-%d ms holds no keyframe (the next one is at %d ms): widen it", e.startMs, e.endMs, e.nextMs)
}

// partPlan is where a part cuts each track: decode ranges [a[i], b[i]) and its window on the source timeline (teNs 0: the end).
type partPlan struct {
	a, b       []int
	tsNs, teNs int64
}

func titleAt(titles []string, i int) string {
	if i < len(titles) {
		return titles[i]
	}
	return ""
}

// planPart finds where range r cuts each track; cur holds, per track, where the search may start and moves forward.
func (m *cutModel) planPart(r mkv.TimeRange, cur []int) (partPlan, error) {
	ref := &m.tracks[m.ref]
	n := len(ref.tt.samples)
	p := partPlan{a: make([]int, len(m.tracks)), b: make([]int, len(m.tracks))}
	a := 0
	if r.StartMs > 0 {
		a = ref.firstSyncFrom(cur[m.ref], m.movieTS, func(pt int64) bool { return roundMs(pt) >= r.StartMs })
		if a < n && ref.ptNs(a, m.movieTS) >= ref.presentEndNs(m.movieTS) {
			a = n // a keyframe the edit list hides opens nothing
		}
		if a == n {
			return p, fmt.Errorf("no keyframe at or after %d ms (the last one is at %d ms): start the range earlier", r.StartMs, lastKeyMs(ref, m.movieTS))
		}
		if r.EndMs > 0 && roundMs(ref.ptNs(a, m.movieTS)) >= r.EndMs {
			return p, &noKeyframeInRange{startMs: r.StartMs, endMs: r.EndMs, nextMs: roundMs(ref.ptNs(a, m.movieTS))}
		}
		p.tsNs = ref.ptNs(a, m.movieTS)
	}
	b := n
	if r.EndMs > 0 {
		b = ref.firstSyncFrom(a+1, m.movieTS, func(pt int64) bool { return roundMs(pt) >= r.EndMs })
	}
	if b < n {
		p.teNs = ref.ptNs(b, m.movieTS)
	}
	for i := range m.tracks {
		c := &m.tracks[i]
		ca, cb := a, b
		if i != m.ref {
			ca, cb = 0, len(c.tt.samples)
			if r.StartMs > 0 {
				ca = c.firstSyncFrom(cur[i], m.movieTS, func(pt int64) bool { return pt >= p.tsNs })
				// A cue on screen at the cut opens the part too, its head hidden by the edit list; the previous part keeps it up to the cut.
				if j := ca - 1; c.isText() && j >= cur[i] && c.ptNs(j, m.movieTS) < p.tsNs && c.ptNs(j, m.movieTS)+rescale(int64(c.tt.samples[j].dur), c.ts, nsPerSec) > p.tsNs {
					ca = j
				}
				if c.presentEndNs(m.movieTS) <= p.tsNs {
					ca = len(c.tt.samples) // a track the edit list has ended by the cut has nothing to show in the part
				}
			}
			if p.teNs > 0 {
				cb = c.firstSyncFrom(ca, m.movieTS, func(pt int64) bool { return pt >= p.teNs })
			}
		}
		p.a[i], p.b[i] = ca, cb
		cur[i] = ca
	}
	return p, nil
}

// writePart writes the part a plan describes, link chaining it to its neighbours.
func (m *cutModel) writePart(ctx context.Context, src io.ReadSeeker, size int64, lay outLayout, p partPlan, link splitLink, outPath string, o Options) error {
	traks := make([][]byte, 0, len(m.tracks))
	var movieDur int64
	for i := range m.tracks {
		c := &m.tracks[i]
		tt := c.tt.slice(p.a[i], p.b[i])
		// Another video track shows its frames up to its own keyframe, where the next part opens it.
		teNs := p.teNs
		if c.otherVideo(i, m.ref) {
			teNs = 0
		}
		edts, dur := partEdit(c, tt, min(p.a[i], len(c.tt.samples)-1), m.movieTS, m.outTS, p.tsNs, teNs)
		t, err := rebuildTrak(c.trak, tt, edts, dur)
		if err != nil {
			return fmt.Errorf("track %d: %w", i+1, err)
		}
		traks = append(traks, t)
		movieDur = max(movieDur, dur)
	}
	// The part runs up to the cut even where no track presents up to it (open-GOP leading pictures the next part hides), or a join would pull the next part early.
	if p.teNs > 0 {
		movieDur = max(movieDur, rescale(p.teNs-p.tsNs, nsPerSec, m.outTS))
	}
	payload, err := m.assemble(traks, movieDur, m.outTS)
	if err != nil {
		return err
	}
	teMs := int64(0)
	if p.teNs > 0 {
		teMs = roundMs(p.teNs)
	}
	vs := vsource{{size: size, r: src}}
	payload, vs, err = withChapters(src, payload, vs, splitplan.ClipChapters(m.meta.Chapters, roundMs(p.tsNs), teMs))
	if err != nil {
		return err
	}
	if payload, err = withSplitLink(payload, link); err != nil {
		return err
	}
	return writeRelocated(ctx, vs, payload, nil, lay, outPath, o)
}

// lastKeyMs is where a track's last shown sync sample presents.
func lastKeyMs(c *cutTrack, movieTS uint32) int64 {
	end := c.presentEndNs(movieTS)
	for i := len(c.tt.samples) - 1; i >= 0; i-- {
		if c.tt.samples[i].sync && c.ptNs(i, movieTS) < end {
			return roundMs(c.ptNs(i, movieTS))
		}
	}
	return 0
}
