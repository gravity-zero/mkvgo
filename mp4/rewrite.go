package mp4

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"

	"github.com/gravity-zero/mkvgo/internal/outguard"
	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/mkv"
)

// Rewrite copies an MP4/MOV to a fast-start file (ftyp, moov, mdat) keeping samples, sample entries and timing byte for byte.
func Rewrite(ctx context.Context, srcPath, dstPath string, opts ...Options) error {
	return rewriteMP4(ctx, srcPath, dstPath, rewriteOptions{}, optionsFrom(opts))
}

// EditMetadata writes srcPath to dstPath with fn's metadata changes, under the rules of EditInPlace.
func EditMetadata(ctx context.Context, srcPath, dstPath string, fn func(c *mkv.Container), opts ...Options) error {
	return rewriteMP4(ctx, srcPath, dstPath, rewriteOptions{edit: fn}, optionsFrom(opts))
}

// SetChapters writes srcPath to dstPath with its chapters replaced (nil removes them).
func SetChapters(ctx context.Context, srcPath, dstPath string, chapters []mkv.Chapter, opts ...Options) error {
	return EditMetadata(ctx, srcPath, dstPath, func(c *mkv.Container) { c.Chapters = chapters }, opts...)
}

// RemoveTrack writes srcPath to dstPath without the given tracks (probe track IDs); the other samples are copied untouched.
func RemoveTrack(ctx context.Context, srcPath, dstPath string, removeIDs []uint64, opts ...Options) error {
	if len(removeIDs) == 0 {
		return errf("remove-track: no track given")
	}
	return rewriteMP4(ctx, srcPath, dstPath, rewriteOptions{remove: removeIDs}, optionsFrom(opts))
}

// rewriteOptions is what a rewrite changes besides the layout.
type rewriteOptions struct {
	remove []uint64               // probe track IDs left out
	edit   func(c *mkv.Container) // metadata edit, as EditInPlace
}

// rwChunk is one chunk carried to the output: where its bytes are read, how many, and where they land.
type rwChunk struct {
	src, size, out int64
	at             int64  // decode time of its first sample (ns), for interleaving sources
	samples        uint32 // samples in the chunk
}

// rwTrak is one output trak and its chunks.
type rwTrak struct {
	payload []byte
	chunks  []rwChunk
}

func rewriteMP4(ctx context.Context, srcPath, dstPath string, ro rewriteOptions, o Options) error {
	if err := distinctOutput(o.FS, dstPath, srcPath); err != nil {
		return err
	}
	src, err := o.FS.DoOpen(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	size, err := src.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	payload, err := readMoovLimit(src, size, o.moovLimit())
	if err != nil {
		return err
	}
	mv, err := parseMoov(payload, size, sampleNone)
	if err != nil {
		return err
	}
	if mv.fragmented {
		return errf("%s: a fragmented MP4 is not rewritten sample by sample; remux it to a progressive MP4 first (from-mp4, then to-mp4)", shown.Path(srcPath))
	}
	vs := vsource{{size: size, r: src}}
	mdats := sourceMdats(src, size)
	if ro.edit != nil {
		var extra []byte
		if payload, extra, err = editedMoov(src, payload, size, mv, ro.edit); err != nil {
			return errf("edit: %w", err)
		}
		vs = vs.withMem(extra)
	}
	drop, err := droppedTraks(mv, ro.remove)
	if err != nil {
		return err
	}
	if err := traksInMdat(payload, drop, mdats, size, vs.limit()); err != nil {
		return errf("%s: %w", shown.Path(srcPath), err)
	}
	ftyp, err := sourceFtyp(src, size)
	if err != nil {
		return err
	}
	top, err := sourceTopBoxes(src, size)
	if err != nil {
		return err
	}
	if len(drop) > 0 {
		if payload, err = withMovieDuration(payload, drop); err != nil {
			return err
		}
	}
	if err := writeRelocated(ctx, vs, payload, drop, outLayout{ftyp: ftyp, top: top}, dstPath, o); err != nil {
		return errf("%s: %w", shown.Path(srcPath), err)
	}
	return nil
}

// distinctOutput refuses an output that is one of the inputs: creating it would truncate what is about to be read.
func distinctOutput(fs *mkv.FS, dstPath string, inputs ...string) error {
	if err := outguard.Distinct(fs.DoStat, dstPath, inputs...); err != nil {
		return errf("%w", err)
	}
	return nil
}

// vseg is one stretch of the virtual byte space chunk offsets point into: a source file, or bytes in memory.
type vseg struct {
	start, size int64
	r           io.ReadSeeker
	mem         []byte
}

// vsource lays sources and in-memory bytes back to back, so one moov can address them all.
type vsource []vseg

func (v vsource) limit() int64 {
	if len(v) == 0 {
		return 0
	}
	last := v[len(v)-1]
	return last.start + last.size
}

// withFile appends a source file of size bytes and returns the offset it starts at.
func (v vsource) withFile(r io.ReadSeeker, size int64) (vsource, int64) {
	start := v.limit()
	return append(v, vseg{start: start, size: size, r: r}), start
}

// withMem appends in-memory bytes (nothing when b is empty).
func (v vsource) withMem(b []byte) vsource {
	if len(b) == 0 {
		return v
	}
	return append(v, vseg{start: v.limit(), size: int64(len(b)), mem: b})
}

// seg returns the index of the segment holding offset off (-1 past the end).
func (v vsource) seg(off int64) int {
	i := sort.Search(len(v), func(i int) bool { return v[i].start+v[i].size > off })
	if i == len(v) || off < v[i].start {
		return -1
	}
	return i
}

// outLayout is how writeRelocated arranges its output around the moov.
type outLayout struct {
	ftyp, top  []byte // written ahead of the moov / between the moov and the mdat
	interleave bool   // chunks ordered by decode time across sources (a mux), not by source offset
}

// writeRelocated writes the fast-start MP4 a moov payload describes: the traks not dropped, their chunks read from vs.
func writeRelocated(ctx context.Context, vs vsource, payload []byte, drop map[int]bool, lay outLayout, dstPath string, o Options) (err error) {
	ftyp := lay.ftyp
	traks, others, err := collectTraks(payload, drop, vs.limit())
	if err != nil {
		return err
	}
	if others, err = restateContentHashes(ctx, vs, traks, others); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if lay.interleave {
		for _, t := range traks {
			setChunkTimes(t)
		}
	}
	var mediaLen int64
	order := make([]*rwChunk, 0)
	for _, t := range traks {
		for i := range t.chunks {
			order = append(order, &t.chunks[i])
			mediaLen += t.chunks[i].size
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		if lay.interleave && order[a].at != order[b].at {
			return order[a].at < order[b].at
		}
		return order[a].src < order[b].src
	})
	co64 := needCo64(mediaLen)
	build := func(base int64) ([]byte, error) {
		pos := base
		for _, c := range order {
			c.out = pos
			pos += c.size
		}
		return buildRewrittenMoov(others, traks, co64)
	}
	probe, err := build(0)
	if err != nil {
		return err
	}
	if !co64 && int64(len(ftyp))+int64(len(probe))+int64(len(lay.top))+mdatHeaderLen+mediaLen > math.MaxUint32 {
		co64 = true // a large moov pushes the media past what 32-bit offsets reach
		if probe, err = build(0); err != nil {
			return err
		}
	}
	moov, err := build(int64(len(ftyp)) + int64(len(probe)) + int64(len(lay.top)) + mdatHeaderLen)
	if err != nil {
		return err
	}
	if len(moov) != len(probe) {
		return errf("rewrite: the moov size moved with its offsets (%d then %d bytes)", len(probe), len(moov))
	}

	fs := o.FS
	dst, err := fs.DoCreate(dstPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := dst.Close(); cerr != nil && err == nil {
			err = errf("close output: %w", cerr)
		}
		if err != nil {
			_ = fs.DoRemove(dstPath)
		}
	}()
	out := bufio.NewWriterSize(dst, 256<<10)
	for _, b := range [][]byte{ftyp, moov, lay.top, mdatHeader(mediaLen)} {
		if _, err := out.Write(b); err != nil {
			return errf("write: %w", err)
		}
	}
	if err := copyChunks(ctx, out, vs, order, mediaLen, o.Progress); err != nil {
		return err
	}
	return out.Flush()
}

// editedMoov applies an edit to a source moov payload; the chapter samples it adds are returned to sit at offset size.
func editedMoov(r io.ReadSeeker, payload []byte, size int64, mv *movie, fn func(*mkv.Container)) ([]byte, []byte, error) {
	before, _, err := moovContainer(r, payload, size)
	if err != nil {
		return nil, nil, err
	}
	after, _, err := moovContainer(r, payload, size)
	if err != nil {
		return nil, nil, err
	}
	fn(after)
	if err := checkEditable(before, after); err != nil {
		return nil, nil, err
	}
	if reflect.DeepEqual(before, after) {
		return payload, nil, nil
	}
	moov, chap, err := patchMoovMeta(payload, mv, before, after, size)
	if err != nil {
		return nil, nil, err
	}
	mb, err := iterBoxes(moov)
	if err != nil || len(mb) != 1 {
		return nil, nil, fmt.Errorf("rebuilt moov does not parse")
	}
	return mb[0].payload, chap, nil
}

// droppedTraks maps the probe track IDs to remove to their trak positions.
func droppedTraks(mv *movie, remove []uint64) (map[int]bool, error) {
	drop := map[int]bool{}
	for _, id := range remove {
		if id == 0 || id > uint64(len(mv.tracks)) {
			return nil, errf("remove-track: track %d not found (the file has %d)", id, len(mv.tracks))
		}
		drop[mv.tracks[id-1].trakIndex] = true
	}
	if len(remove) > 0 && len(drop) == len(mv.tracks) {
		return nil, errf("remove-track: removing every track leaves nothing to write")
	}
	return drop, nil
}

// collectTraks splits a moov payload into its kept traks (with their chunks) and its other children.
func collectTraks(payload []byte, drop map[int]bool, limit int64) (traks []*rwTrak, others []memBox, err error) {
	boxes, err := iterBoxes(payload)
	if err != nil {
		return nil, nil, err
	}
	removedIDs := map[uint32]bool{}
	idx := -1
	for _, b := range boxes {
		if b.typ != "trak" {
			continue
		}
		idx++
		if drop[idx] {
			id, _ := trakID(b.payload)
			removedIDs[id] = true
		}
	}
	for id := range danglingRefs(boxes) {
		removedIDs[id] = true
	}
	chapIDs := chapterTrackRefs(boxes)
	idx = -1
	var kept []memBox
	var keptIdx []int
	for _, b := range boxes {
		if b.typ != "trak" {
			others = append(others, b)
			continue
		}
		idx++
		if drop[idx] {
			continue
		}
		p, err := dropTrefIDs(b.payload, removedIDs)
		if err != nil {
			return nil, nil, err
		}
		kept = append(kept, memBox{typ: "trak", payload: p})
		keptIdx = append(keptIdx, idx)
	}
	names := map[int]int{} // trak position -> probe track ID, to name a refused track the way the user knows it
	if mv, err := parseMoov(payload, limit, sampleNone); err == nil {
		for i := range mv.tracks {
			names[mv.tracks[i].trakIndex] = i + 1
		}
	}
	name := func(i int, b memBox) string {
		if n, ok := names[keptIdx[i]]; ok {
			return fmt.Sprintf("track %d", n)
		}
		id, _ := trakID(b.payload)
		return fmt.Sprintf("the trak with track_ID %d (not a media track)", id)
	}
	// A chapter track no kept trak points at any more is pointed at again from the first kept audio or video trak.
	stillRef := chapterTrackRefs(kept)
	for id := range chapIDs {
		if stillRef[id] || removedIDs[id] {
			continue
		}
		for i, b := range kept {
			if tb, err := iterBoxes(b.payload); err == nil && (trakHandler(tb) == "vide" || trakHandler(tb) == "soun") {
				p, err := patchTrakChapRef(b.payload, nil, true, id)
				if err != nil {
					return nil, nil, err
				}
				kept[i].payload = p
				stillRef[id] = true
				break
			}
		}
	}
	for i, b := range kept {
		if id, _ := trakID(b.payload); chapIDs[id] && !stillRef[id] {
			continue
		}
		if err := relocatable(b.payload); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name(i, b), err)
		}
		chunks, err := trakChunks(b.payload, limit)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name(i, b), err)
		}
		traks = append(traks, &rwTrak{payload: b.payload, chunks: chunks})
	}
	return traks, others, nil
}

// relocatable refuses a trak whose sample data a chunk-offset rewrite cannot move: external data references, CENC auxiliary data.
func relocatable(trak []byte) error {
	minf, err := boxAt(trak, "mdia", "minf")
	if err != nil {
		return err
	}
	if dinf, err := boxAt(minf, "dinf", "dref"); err == nil && len(dinf) >= 8 {
		entries, err := iterBoxes(dinf[8:])
		if err != nil {
			return err
		}
		for _, e := range entries {
			if len(e.payload) < 4 || e.payload[3]&1 == 0 {
				return fmt.Errorf("its samples live in another file (data reference %q): only a self-contained MP4 can be rewritten", e.typ)
			}
		}
	}
	stbl, err := boxAt(minf, "stbl")
	if err != nil {
		return err
	}
	sb, err := iterBoxes(stbl)
	if err != nil {
		return err
	}
	if hasMemBox(sb, "saio") {
		return fmt.Errorf("its encryption data (saio) points into the file and cannot be moved: mkvgo does not rewrite encrypted MP4")
	}
	return nil
}

// boxAt descends from a box payload through the named children and returns the last one's payload.
func boxAt(payload []byte, path ...string) ([]byte, error) {
	for _, typ := range path {
		boxes, err := iterBoxes(payload)
		if err != nil {
			return nil, err
		}
		b, ok := findMemBox(boxes, typ)
		if !ok {
			return nil, fmt.Errorf("no %s box", typ)
		}
		payload = b.payload
	}
	return payload, nil
}

// rewriteAt rebuilds payload with the children of the box at path replaced by leaf's result.
func rewriteAt(payload []byte, path []string, leaf func([]memBox) ([][]byte, error)) ([]byte, error) {
	boxes, err := iterBoxes(payload)
	if err != nil {
		return nil, err
	}
	if len(path) == 0 {
		kids, err := leaf(boxes)
		if err != nil {
			return nil, err
		}
		return joinBoxes(kids), nil
	}
	out := make([][]byte, 0, len(boxes))
	found := false
	for _, b := range boxes {
		if b.typ != path[0] || found {
			out = append(out, box(b.typ, b.payload))
			continue
		}
		found = true
		p, err := rewriteAt(b.payload, path[1:], leaf)
		if err != nil {
			return nil, err
		}
		out = append(out, box(b.typ, p))
	}
	if !found {
		return nil, fmt.Errorf("no %s box", path[0])
	}
	return joinBoxes(out), nil
}

// sourceMdats locates a source's mdat payloads; a file whose top-level walk fails (junk past the moov) gets nil: bounds alone apply.
func sourceMdats(r io.ReadSeeker, size int64) [][2]int64 {
	ranges, err := mdatRanges(r, size)
	if err != nil {
		return nil
	}
	return ranges
}

// inMdats reports whether [off, off+n) lies inside one of the ascending ranges (nil: no ranges to hold to).
func inMdats(ranges [][2]int64, off, n int64) bool {
	if ranges == nil {
		return true
	}
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i][1] > off })
	return i < len(ranges) && inMdat(ranges[i:i+1], off, n)
}

// checkInMdat refuses a trak whose chunks below real (the source's own bytes) lie outside every mdat, such as in the ftyp.
func checkInMdat(chunks []rwChunk, mdats [][2]int64, real int64) error {
	for i, c := range chunks {
		if c.size > 0 && c.src < real && !inMdats(mdats, c.src, c.size) {
			return fmt.Errorf("chunk %d at %d lies outside every mdat: the file is damaged, remux it with a tool that rebuilds its tables", i+1, c.src)
		}
	}
	return nil
}

// trakChunks lists a trak's chunks with their byte sizes, each within limit bytes.
func trakChunks(trak []byte, limit int64) ([]rwChunk, error) {
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
	offsets, err := parseChunkOffsets(sb)
	if err != nil {
		return nil, err
	}
	stsc, ok := findMemBox(sb, "stsc")
	if !ok {
		return nil, fmt.Errorf("stbl without stsc")
	}
	entries, err := parseStsc(stsc.payload)
	if err != nil {
		return nil, err
	}
	chunks := make([]rwChunk, len(offsets))
	si, ei := 0, 0
	for ci, off := range offsets {
		chunk := uint32(ci + 1)
		for ei+1 < len(entries) && entries[ei+1].firstChunk <= chunk {
			ei++
		}
		var perChunk uint32
		if len(entries) > 0 && chunk >= entries[ei].firstChunk {
			perChunk = entries[ei].perChunk
		}
		var n int64
		for k := uint32(0); k < perChunk && si < len(sizes); k++ {
			n += int64(sizes[si])
			si++
		}
		if n > 0 && (off < 0 || off+n > limit) {
			return nil, fmt.Errorf("chunk %d at [%d:%d] is outside the file (size %d)", chunk, off, off+n, limit)
		}
		chunks[ci] = rwChunk{src: off, size: n, samples: perChunk}
	}
	if si != len(sizes) {
		return nil, fmt.Errorf("chunk table covers %d samples but stsz lists %d", si, len(sizes))
	}
	return chunks, nil
}

// danglingRefs is the set of track_IDs some trak's tref names but no trak in the moov carries.
func danglingRefs(boxes []memBox) map[uint32]bool {
	present := map[uint32]bool{}
	for _, b := range boxes {
		if b.typ == "trak" {
			id, _ := trakID(b.payload)
			present[id] = true
		}
	}
	gone := map[uint32]bool{}
	for _, b := range boxes {
		if b.typ != "trak" {
			continue
		}
		tb, err := iterBoxes(b.payload)
		if err != nil {
			continue
		}
		tref, ok := findMemBox(tb, "tref")
		if !ok {
			continue
		}
		refs, err := iterBoxes(tref.payload)
		if err != nil {
			continue
		}
		for _, r := range refs {
			for off := 0; off+4 <= len(r.payload); off += 4 {
				if id := binary.BigEndian.Uint32(r.payload[off:]); !present[id] {
					gone[id] = true
				}
			}
		}
	}
	return gone
}

// dropTrefIDs removes the references to the given track_IDs from a trak's tref.
func dropTrefIDs(trak []byte, ids map[uint32]bool) ([]byte, error) {
	if len(ids) == 0 {
		return trak, nil
	}
	tb, err := iterBoxes(trak)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(tb))
	for _, b := range tb {
		if b.typ != "tref" {
			out = append(out, box(b.typ, b.payload))
			continue
		}
		refs, err := iterBoxes(b.payload)
		if err != nil {
			return nil, err
		}
		var kept [][]byte
		for _, r := range refs {
			var w bw
			for off := 0; off+4 <= len(r.payload); off += 4 {
				if id := binary.BigEndian.Uint32(r.payload[off:]); !ids[id] {
					w.u32(id)
				}
			}
			if len(w.b) > 0 {
				kept = append(kept, box(r.typ, w.b))
			}
		}
		if len(kept) > 0 {
			out = append(out, container("tref", kept...))
		}
	}
	return joinBoxes(out), nil
}

// buildRewrittenMoov assembles the output moov: the other children verbatim, each trak with its chunk offsets relocated.
func buildRewrittenMoov(others []memBox, traks []*rwTrak, co64 bool) ([]byte, error) {
	var w bw
	placed := false
	emitTraks := func() error {
		for _, t := range traks {
			p, err := rewriteAt(t.payload, []string{"mdia", "minf", "stbl"}, func(sb []memBox) ([][]byte, error) {
				out := make([][]byte, 0, len(sb))
				for _, b := range sb {
					if b.typ == "stco" || b.typ == "co64" {
						out = append(out, chunkOffsetBox(t.chunks, co64))
						continue
					}
					out = append(out, box(b.typ, b.payload))
				}
				return out, nil
			})
			if err != nil {
				return err
			}
			w.bytes(box("trak", p))
		}
		placed = true
		return nil
	}
	for _, b := range others {
		w.bytes(box(b.typ, b.payload))
		if b.typ == "mvhd" && !placed {
			if err := emitTraks(); err != nil {
				return nil, err
			}
		}
	}
	if !placed {
		if err := emitTraks(); err != nil {
			return nil, err
		}
	}
	return box("moov", w.b), nil
}

// chunkOffsetBox builds the stco (or co64) of the chunks' output offsets.
func chunkOffsetBox(chunks []rwChunk, co64 bool) []byte {
	if co64 {
		return fullBox("co64", 0, 0, func(w *bw) {
			w.u32(uint32(len(chunks)))
			for _, c := range chunks {
				w.u64(uint64(c.out))
			}
		})
	}
	return fullBox("stco", 0, 0, func(w *bw) {
		w.u32(uint32(len(chunks)))
		for _, c := range chunks {
			w.u32(uint32(c.out))
		}
	})
}

// sourceFtyp returns the source's leading ftyp box verbatim (nil when it has none).
func sourceFtyp(r io.ReadSeeker, size int64) ([]byte, error) {
	if size < 8 {
		return nil, nil
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int64(binary.BigEndian.Uint32(hdr[:4]))
	if string(hdr[4:8]) != "ftyp" || n < 8 || n > 1<<20 || n > size {
		return nil, nil
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return readExact(r, n)
}

// copyRunMax bounds one coalesced read, so cancellation and progress keep their pace on a contiguous file.
var copyRunMax int64 = 8 << 20

// copyChunks streams the chunks in output order, coalescing the ones adjacent in the source into one read.
func copyChunks(ctx context.Context, w io.Writer, vs vsource, order []*rwChunk, total int64, progress mkv.ProgressFunc) error {
	var done int64
	for i := 0; i < len(order); {
		if err := ctx.Err(); err != nil {
			return err
		}
		start, n := order[i].src, order[i].size
		if n == 0 {
			i++
			continue
		}
		si := vs.seg(start)
		if si < 0 {
			return errf("chunk at %d lies past the sources", start)
		}
		seg := vs[si]
		j := i + 1
		for j < len(order) && n < copyRunMax && order[j].src == start+n && start+n+order[j].size <= seg.start+seg.size {
			n += order[j].size
			j++
		}
		if start+n > seg.start+seg.size {
			return errf("chunk at %d runs past its source", start)
		}
		off := start - seg.start
		if seg.mem != nil {
			if _, err := w.Write(seg.mem[off : off+n]); err != nil {
				return errf("write mdat: %w", err)
			}
		} else {
			if _, err := seg.r.Seek(off, io.SeekStart); err != nil {
				return err
			}
			if _, err := io.CopyN(w, seg.r, n); err != nil {
				return errf("copy samples at %d: %w", off, err)
			}
		}
		done += n
		if progress != nil {
			progress(done, total)
		}
		i = j
	}
	return nil
}

// sourceTopBoxes returns, verbatim, the top-level boxes a rewrite carries besides ftyp, moov and mdat: XMP uuid, user data,
// QuickTime preview, and a meta that addresses nothing by file offset (no iloc).
func sourceTopBoxes(r io.ReadSeeker, size int64) ([]byte, error) {
	const keepMax = 16 << 20
	var out []byte
	var hdr [16]byte
	for off := int64(0); off+8 <= size; {
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			return nil, err
		}
		n, typ, hl := int64(binary.BigEndian.Uint32(hdr[:4])), string(hdr[4:8]), int64(8)
		if n == 1 {
			if _, err := io.ReadFull(r, hdr[8:16]); err != nil {
				return nil, err
			}
			n, hl = int64(binary.BigEndian.Uint64(hdr[8:16])), 16
		}
		if n == 0 {
			n = size - off
		}
		if n < hl || off+n > size {
			break // past a damaged box nothing more is carried; the media was located without it
		}
		switch typ {
		case "uuid", "udta", "pnot", "meta":
			if int64(len(out))+n > keepMax {
				return nil, errf("its top-level %q boxes pass %d MiB: rewrite it with a tool that drops them", typ, keepMax>>20)
			}
			if _, err := r.Seek(off, io.SeekStart); err != nil {
				return nil, err
			}
			b, err := readExact(r, n)
			if err != nil {
				return nil, err
			}
			if typ == "meta" && bytes.Contains(b, []byte("iloc")) {
				break
			}
			out = append(out, b...)
		}
		off += n
	}
	return out, nil
}

// traksInMdat checks every kept trak's chunks against the source's mdats (chunks past real are the edit's own bytes).
func traksInMdat(payload []byte, drop map[int]bool, mdats [][2]int64, real, limit int64) error {
	boxes, err := iterBoxes(payload)
	if err != nil {
		return err
	}
	idx := -1
	for _, b := range boxes {
		if b.typ != "trak" {
			continue
		}
		if idx++; drop[idx] {
			continue
		}
		chunks, err := trakChunks(b.payload, limit)
		if err != nil {
			return err
		}
		if err := checkInMdat(chunks, mdats, real); err != nil {
			id, _ := trakID(b.payload)
			return fmt.Errorf("track_ID %d: %w", id, err)
		}
	}
	return nil
}

// withMovieDuration recomputes the mvhd duration over the media traks a removal keeps: a chapter or timecode track runs to the old end.
func withMovieDuration(payload []byte, drop map[int]bool) ([]byte, error) {
	boxes, err := iterBoxes(payload)
	if err != nil {
		return nil, err
	}
	chapters := chapterTrackRefs(boxes)
	var dur int64
	idx := -1
	for _, b := range boxes {
		if b.typ != "trak" {
			continue
		}
		idx++
		tb, _ := iterBoxes(b.payload)
		if id, _ := trakID(b.payload); chapters[id] || trakHandler(tb) == "tmcd" {
			continue
		}
		if !drop[idx] {
			if d, err := trakDuration(b.payload); err == nil {
				dur = max(dur, d)
			}
		}
	}
	var w bw
	for _, b := range boxes {
		p := b.payload
		if b.typ == "mvhd" {
			if p, err = patchMvhdDuration(p, dur); err != nil {
				return nil, err
			}
		}
		w.bytes(box(b.typ, p))
	}
	return w.b, nil
}

// setChunkTimes stamps each chunk with its first sample's decode time; a table it cannot read leaves the times at 0.
func setChunkTimes(t *rwTrak) {
	stbl, err := boxAt(t.payload, "mdia", "minf", "stbl")
	if err != nil {
		return
	}
	sb, err := iterBoxes(stbl)
	if err != nil {
		return
	}
	var n int
	for _, c := range t.chunks {
		n += int(c.samples)
	}
	durs, err := parseStts(mustFind(sb, "stts"), n)
	mdhd, merr := boxAt(t.payload, "mdia", "mdhd")
	if err != nil || merr != nil {
		return
	}
	ts, _ := parseMdhd(mdhd)
	var dts int64
	si := 0
	for i := range t.chunks {
		t.chunks[i].at = rescale(dts, ts, nsPerSec)
		for k := uint32(0); k < t.chunks[i].samples && si < len(durs); k++ {
			dts += int64(durs[si])
			si++
		}
	}
}
