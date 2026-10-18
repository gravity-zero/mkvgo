package mp4

import (
	"encoding/binary"
	"fmt"
	"maps"
	"math"
	"math/bits"
	"slices"
)

// rwSample is one sample a split or join carries: its bytes (virtual offset), its timing in track ticks and its flags.
type rwSample struct {
	src  int64
	dts  int64
	size uint32
	dur  uint32
	cto  int32
	sdi  uint32
	sync bool
}

// trakTable is a trak's samples with the per-sample side tables carried along.
type trakTable struct {
	samples []rwSample
	sdtp    []byte              // per-sample dependency flags; nil when the trak has none
	groups  map[string][]uint32 // sbgp grouping_type -> per-sample group_description_index
	gparam  map[string]*uint32  // sbgp grouping_type_parameter (version 1 boxes)
}

// readTrakTable expands a trak's sample table, base added to every chunk offset, its samples taken from budget.
func readTrakTable(trak []byte, base, limit int64, budget *int64) (*trakTable, error) {
	stbl, err := boxAt(trak, "mdia", "minf", "stbl")
	if err != nil {
		return nil, err
	}
	sb, err := iterBoxes(stbl)
	if err != nil {
		return nil, err
	}
	stsz, ok := findMemBox(sb, "stsz")
	if !ok {
		return nil, noStsz(sb)
	}
	sizes, err := parseStsz(stsz.payload, limit)
	if err != nil {
		return nil, err
	}
	n := len(sizes)
	if int64(n) > *budget {
		return nil, fmt.Errorf("its %d samples pass the %d the files may hold together", n, maxSamples)
	}
	*budget -= int64(n)
	offsets, err := parseChunkOffsets(sb)
	if err != nil {
		return nil, err
	}
	stsc, ok := findMemBox(sb, "stsc")
	if !ok {
		return nil, fmt.Errorf("stbl without stsc")
	}
	entries, err := parseStscSDI(stsc.payload)
	if err != nil {
		return nil, err
	}
	durs, err := parseStts(mustFind(sb, "stts"), n)
	if err != nil {
		return nil, err
	}
	ctos := parseCtts(sb, n)
	syncSet := parseStss(sb)

	tt := &trakTable{samples: make([]rwSample, n)}
	si, ei := 0, 0
	for ci, off := range offsets {
		chunk := uint32(ci + 1)
		for ei+1 < len(entries) && entries[ei+1].firstChunk <= chunk {
			ei++
		}
		var per, sdi uint32
		if len(entries) > 0 && chunk >= entries[ei].firstChunk {
			per, sdi = entries[ei].perChunk, entries[ei].sdi
		}
		pos := off
		for k := uint32(0); k < per && si < n; k++ {
			if pos < 0 || pos+int64(sizes[si]) > limit-base {
				return nil, fmt.Errorf("sample %d at [%d:%d] is outside the file", si, pos, pos+int64(sizes[si]))
			}
			tt.samples[si] = rwSample{src: base + pos, size: sizes[si], sdi: sdi}
			pos += int64(sizes[si])
			si++
		}
	}
	if si != n {
		return nil, fmt.Errorf("chunk table covers %d samples but stsz lists %d", si, n)
	}
	var dts int64
	for i := range tt.samples {
		s := &tt.samples[i]
		s.dts, s.dur, s.cto = dts, durs[i], ctos[i]
		s.sync = syncSet == nil || syncSet[i+1]
		dts += int64(durs[i])
	}
	if b, ok := findMemBox(sb, "sdtp"); ok && len(b.payload) >= 4+n {
		tt.sdtp = append([]byte(nil), b.payload[4:4+n]...)
	}
	for _, b := range sb {
		if b.typ != "sbgp" {
			continue
		}
		typ, param, idx, err := parseSbgp(b.payload, n)
		if err != nil {
			return nil, err
		}
		if tt.groups == nil {
			tt.groups, tt.gparam = map[string][]uint32{}, map[string]*uint32{}
		}
		tt.groups[typ], tt.gparam[typ] = idx, param
	}
	return tt, nil
}

// noStsz explains a sample table without stsz.
func noStsz(sb []memBox) error {
	if hasMemBox(sb, "stz2") {
		return fmt.Errorf("its sample sizes are in a compact stz2 table, which mkvgo does not read: remux it to MKV (from-mp4) to work on it")
	}
	return fmt.Errorf("stbl without stsz")
}

type stscSDI struct {
	firstChunk, perChunk, sdi uint32
}

// parseStscSDI parses stsc keeping each entry's sample_description_index.
func parseStscSDI(payload []byte) ([]stscSDI, error) {
	if len(payload) < 8 {
		return nil, fmt.Errorf("stsc too short")
	}
	count := binary.BigEndian.Uint32(payload[4:8])
	if uint64(len(payload)) < 8+uint64(count)*12 {
		return nil, fmt.Errorf("stsc declares %d entries but is too short", count)
	}
	out := make([]stscSDI, count)
	for i := range out {
		p := payload[8+i*12:]
		out[i] = stscSDI{binary.BigEndian.Uint32(p), binary.BigEndian.Uint32(p[4:]), binary.BigEndian.Uint32(p[8:])}
	}
	return out, nil
}

// parseSbgp expands a sample-to-group box into one group_description_index per sample (0: no group).
func parseSbgp(p []byte, n int) (typ string, param *uint32, idx []uint32, err error) {
	if len(p) < 12 {
		return "", nil, nil, fmt.Errorf("sbgp too short")
	}
	typ = string(p[4:8])
	off := 8
	if p[0] == 1 {
		v := binary.BigEndian.Uint32(p[8:12])
		param = &v
		off = 12
	}
	if len(p) < off+4 {
		return "", nil, nil, fmt.Errorf("sbgp too short")
	}
	count := binary.BigEndian.Uint32(p[off:])
	off += 4
	if uint64(len(p)) < uint64(off)+uint64(count)*8 {
		return "", nil, nil, fmt.Errorf("sbgp declares %d entries but is too short", count)
	}
	idx = make([]uint32, n)
	at := 0
	for i := uint32(0); i < count && at < n; i++ {
		run := int(min(binary.BigEndian.Uint32(p[off:]), uint32(n-at)))
		gdi := binary.BigEndian.Uint32(p[off+4:])
		for k := 0; k < run; k++ {
			idx[at+k] = gdi
		}
		at += run
		off += 8
	}
	return typ, param, idx, nil
}

// slice returns the samples [a,b) with their side tables, decode times rebased to the first.
func (tt *trakTable) slice(a, b int) *trakTable {
	out := &trakTable{samples: append([]rwSample(nil), tt.samples[a:b]...)}
	if tt.sdtp != nil {
		out.sdtp = tt.sdtp[a:b]
	}
	if tt.groups != nil {
		out.groups, out.gparam = map[string][]uint32{}, tt.gparam
		for typ, idx := range tt.groups {
			out.groups[typ] = idx[a:b]
		}
	}
	if len(out.samples) > 0 {
		d0 := out.samples[0].dts
		for i := range out.samples {
			out.samples[i].dts -= d0
		}
	}
	return out
}

// mediaDur is the sum of the sample durations.
func (tt *trakTable) mediaDur() int64 {
	if len(tt.samples) == 0 {
		return 0
	}
	last := tt.samples[len(tt.samples)-1]
	return last.dts + int64(last.dur)
}

// minCts is the earliest composition time.
func (tt *trakTable) minCts() int64 {
	m := int64(math.MaxInt64)
	for _, s := range tt.samples {
		m = min(m, s.dts+int64(s.cto))
	}
	if m == math.MaxInt64 {
		return 0
	}
	return m
}

// maxEnd is the latest composition end.
func (tt *trakTable) maxEnd() int64 {
	var m int64
	for _, s := range tt.samples {
		m = max(m, s.dts+int64(s.cto)+int64(s.dur))
	}
	return m
}

// stblChildren builds the sample table boxes, chunk offsets set to the samples' virtual offsets; stsd and sgpds are carried verbatim.
func (tt *trakTable) stblChildren(stsd []byte, sgpds [][]byte) [][]byte {
	s := tt.samples
	out := [][]byte{box("stsd", stsd)}
	out = append(out, fullBox("stts", 0, 0, func(w *bw) {
		var runs [][2]uint32
		for _, x := range s {
			if k := len(runs); k > 0 && runs[k-1][1] == x.dur {
				runs[k-1][0]++
			} else {
				runs = append(runs, [2]uint32{1, x.dur})
			}
		}
		w.u32(uint32(len(runs)))
		for _, r := range runs {
			w.u32(r[0])
			w.u32(r[1])
		}
	}))
	needCtts, negative := false, false
	for _, x := range s {
		needCtts = needCtts || x.cto != 0
		negative = negative || x.cto < 0
	}
	if needCtts {
		ver := uint8(0)
		if negative {
			ver = 1
		}
		out = append(out, fullBox("ctts", ver, 0, func(w *bw) {
			var runs [][2]uint32
			for _, x := range s {
				if k := len(runs); k > 0 && runs[k-1][1] == uint32(x.cto) {
					runs[k-1][0]++
				} else {
					runs = append(runs, [2]uint32{1, uint32(x.cto)})
				}
			}
			w.u32(uint32(len(runs)))
			for _, r := range runs {
				w.u32(r[0])
				w.u32(r[1])
			}
		}))
	}
	allSync := true
	for _, x := range s {
		allSync = allSync && x.sync
	}
	if !allSync {
		out = append(out, fullBox("stss", 0, 0, func(w *bw) {
			var idx []uint32
			for i, x := range s {
				if x.sync {
					idx = append(idx, uint32(i+1))
				}
			}
			w.u32(uint32(len(idx)))
			for _, v := range idx {
				w.u32(v)
			}
		}))
	}
	if tt.sdtp != nil {
		out = append(out, fullBox("sdtp", 0, 0, func(w *bw) { w.bytes(tt.sdtp) }))
	}
	out = append(out, fullBox("stsz", 0, 0, func(w *bw) {
		uniform := len(s) > 0
		for _, x := range s {
			uniform = uniform && x.size == s[0].size
		}
		if uniform {
			w.u32(s[0].size)
			w.u32(uint32(len(s)))
			return
		}
		w.u32(0)
		w.u32(uint32(len(s)))
		for _, x := range s {
			w.u32(x.size)
		}
	}))
	chunks := tt.chunkRuns()
	out = append(out, fullBox("stsc", 0, 0, func(w *bw) {
		var rows []stscSDI
		for i, c := range chunks {
			if k := len(rows); k > 0 && rows[k-1].perChunk == c.count && rows[k-1].sdi == c.sdi {
				continue
			}
			rows = append(rows, stscSDI{uint32(i + 1), c.count, c.sdi})
		}
		w.u32(uint32(len(rows)))
		for _, r := range rows {
			w.u32(r.firstChunk)
			w.u32(r.perChunk)
			w.u32(r.sdi)
		}
	}))
	out = append(out, fullBox("co64", 0, 0, func(w *bw) {
		w.u32(uint32(len(chunks)))
		for _, c := range chunks {
			w.u64(uint64(c.src))
		}
	}))
	out = append(out, sgpds...)
	for _, typ := range slices.Sorted(maps.Keys(tt.groups)) {
		idx, param := tt.groups[typ], tt.gparam[typ]
		ver := uint8(0)
		if param != nil {
			ver = 1
		}
		out = append(out, fullBox("sbgp", ver, 0, func(w *bw) {
			w.fourcc(typ)
			if param != nil {
				w.u32(*param)
			}
			var runs [][2]uint32
			for _, g := range idx {
				if k := len(runs); k > 0 && runs[k-1][1] == g {
					runs[k-1][0]++
				} else {
					runs = append(runs, [2]uint32{1, g})
				}
			}
			w.u32(uint32(len(runs)))
			for _, r := range runs {
				w.u32(r[0])
				w.u32(r[1])
			}
		}))
	}
	return out
}

type chunkRun struct {
	src   int64
	count uint32
	sdi   uint32
}

// chunkRuns groups consecutive samples whose bytes are contiguous (same description, bounded) into chunks.
func (tt *trakTable) chunkRuns() []chunkRun {
	var out []chunkRun
	var end, bytes int64
	for _, x := range tt.samples {
		k := len(out)
		if k > 0 && x.src == end && x.sdi == out[k-1].sdi && out[k-1].count < chunkSampleThreshold && bytes < chunkByteThreshold {
			out[k-1].count++
		} else {
			out = append(out, chunkRun{src: x.src, count: 1, sdi: x.sdi})
			bytes = 0
		}
		end = x.src + int64(x.size)
		bytes += int64(x.size)
	}
	return out
}

// simpleEdit is a trak's edit list in its common form: an optional empty edit, then one media edit at rate 1.
type simpleEdit struct {
	empty     int64 // movie ticks
	mediaTime int64 // media ticks
	segDur    int64 // movie ticks of the media edit; -1 when the list leaves it open (no edit list)
}

// trakSimpleEdit reads a trak's edit list, refusing the forms a cut cannot carry (several media edits, a rate other than 1).
func trakSimpleEdit(trakBoxes []memBox) (simpleEdit, error) {
	entries, err := trakElstEntries(trakBoxes)
	if err != nil {
		return simpleEdit{}, err
	}
	e := simpleEdit{segDur: -1}
	media := 0
	for _, x := range entries {
		if x.mediaTime < 0 {
			if media > 0 {
				return e, fmt.Errorf("its edit list pauses the media midway (an empty edit after a media one), which a cut cannot carry: remux it to MKV (from-mp4) and cut there")
			}
			e.empty += x.segDur
			continue
		}
		media++
		if media > 1 {
			return e, fmt.Errorf("its edit list plays several pieces of media, which a cut cannot carry: remux it to MKV (from-mp4) and cut there")
		}
		if x.rate != 1<<16 {
			return e, fmt.Errorf("its edit list plays the media at rate %.3f, which a cut cannot carry: remux it to MKV (from-mp4) and cut there", float64(x.rate)/65536)
		}
		e.mediaTime, e.segDur = x.mediaTime, x.segDur
	}
	return e, nil
}

// rescale converts v ticks at timescale from into timescale to, rounding half away from zero.
func rescale(v int64, from, to uint32) int64 {
	if from == to || from == 0 {
		return v
	}
	neg := v < 0
	if neg {
		v = -v
	}
	hi, lo := bits.Mul64(uint64(v), uint64(to))
	lo, carry := bits.Add64(lo, uint64(from)/2, 0)
	hi += carry
	if hi >= uint64(from) {
		return math.MaxInt64
	}
	q, _ := bits.Div64(hi, lo, uint64(from))
	r := int64(min(q, math.MaxInt64))
	if neg {
		return -r
	}
	return r
}

// buildEdit frames an edit list: an empty edit of empty movie ticks (when positive) then the media edit.
func buildEdit(empty, mediaTime, segDur int64) []byte {
	var entries []elstEntry
	if empty > 0 {
		entries = append(entries, elstEntry{segDur: empty, mediaTime: -1, rate: 1 << 16})
	}
	entries = append(entries, elstEntry{segDur: max(segDur, 0), mediaTime: mediaTime, rate: 1 << 16})
	return buildElstBox(entries)
}

// rebuildTrak rebuilds a trak around a new sample table: tkhd and mdhd durations, edit list, stbl; the rest verbatim.
func rebuildTrak(trak []byte, tt *trakTable, edts []byte, tkhdDur int64) ([]byte, error) {
	tb, err := iterBoxes(trak)
	if err != nil {
		return nil, err
	}
	stbl, err := boxAt(trak, "mdia", "minf", "stbl")
	if err != nil {
		return nil, err
	}
	sb, err := iterBoxes(stbl)
	if err != nil {
		return nil, err
	}
	stsd, ok := findMemBox(sb, "stsd")
	if !ok {
		return nil, fmt.Errorf("stbl without stsd")
	}
	var sgpds [][]byte
	for _, b := range sb {
		if b.typ == "sgpd" {
			sgpds = append(sgpds, box("sgpd", b.payload))
		}
	}
	kids := tt.stblChildren(stsd.payload, sgpds)
	out := make([][]byte, 0, len(tb)+1)
	for _, b := range tb {
		switch b.typ {
		case "edts":
			continue
		case "tkhd":
			p, err := patchTkhdDuration(b.payload, tkhdDur)
			if err != nil {
				return nil, err
			}
			out = append(out, box("tkhd", p))
			if edts != nil {
				out = append(out, edts)
			}
		case "mdia":
			p, err := rewriteAt(b.payload, nil, func(mb []memBox) ([][]byte, error) {
				var kids2 [][]byte
				for _, m := range mb {
					switch m.typ {
					case "mdhd":
						p, err := patchMdhdDuration(m.payload, tt.mediaDur())
						if err != nil {
							return nil, err
						}
						kids2 = append(kids2, box("mdhd", p))
					case "minf":
						p, err := rewriteAt(m.payload, []string{"stbl"}, func([]memBox) ([][]byte, error) { return kids, nil })
						if err != nil {
							return nil, err
						}
						kids2 = append(kids2, box("minf", p))
					default:
						kids2 = append(kids2, box(m.typ, m.payload))
					}
				}
				return kids2, nil
			})
			if err != nil {
				return nil, err
			}
			out = append(out, box("mdia", p))
		default:
			out = append(out, box(b.typ, b.payload))
		}
	}
	return joinBoxes(out), nil
}
