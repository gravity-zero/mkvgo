package mp4

import (
	"context"
	"crypto/aes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gravity-zero/mkvgo/internal/fileid"
	"github.com/gravity-zero/mkvgo/mkv"
)

// tableCacheBytes bounds the window tables one plan keeps: a table is the video's structure plus the window's audio segments, a few MiB at most.
const tableCacheBytes = 16 << 20

// tableTotalBudget bounds the tables every plan of the process keeps together (SetTableCacheBytes); tableBytesTotal is what they hold now.
var (
	tableTotalBudget atomic.Int64
	tableBytesTotal  atomic.Int64
)

func init() { tableTotalBudget.Store(128 << 20) }

// SetTableCacheBytes caps, process-wide, the window tables all plans keep for Open; plans over it drop their oldest. Zero restores the default of 128 MiB.
func SetTableCacheBytes(total int64) {
	if total <= 0 {
		total = 128 << 20
	}
	tableTotalBudget.Store(total)
}

// tableAccount is a plan's share of tableBytesTotal, given back when the plan is collected.
type tableAccount struct{ bytes int64 }

func newTableAccount(p *HLSPlan) *tableAccount {
	a := &tableAccount{}
	runtime.AddCleanup(p, func(a *tableAccount) { tableBytesTotal.Add(-a.bytes) }, a)
	return a
}

// spanMergeGap is the largest hole between two payloads the copier reads
// through rather than seeks over.
const spanMergeGap = 8 << 10

// streamBufBytes is the copier's buffer: what a streamed segment costs a client.
const streamBufBytes = 256 << 10

var streamBufs = sync.Pool{New: func() any { b := make([]byte, streamBufBytes); return &b }}

// sendfileMin is the span length from which the copier hands the source file itself to a writer that takes one (sendfile on a socket); shorter spans go through the buffer.
const sendfileMin = 16 << 10

// stageBytes is how much the copier gathers before one write.
const stageBytes = 64 << 10

var stageBufs = sync.Pool{New: func() any { b := make([]byte, 0, stageBytes+64<<10); return &b }}

// windowTable is one window's structure: every rendition timed, its segment
// head framed, and where each sample's bytes sit in the source - enough to
// write any of its segments from the source without holding one.
type windowTable struct {
	segs  []trackSegment
	heads [][]byte
	offs  [][]int64
	bytes int64
}

type tableFlight struct {
	*flight
	table *windowTable
	err   error
}

// streams reports whether this plan can write segments from the source: the
// served bytes must be the source's own, encrypted or not (no conversion),
// from a file the plan may read twice.
func (p *HLSPlan) streams() bool {
	if p.opts.FS != nil && !p.opts.StreamFromFS {
		return false
	}
	for _, pt := range p.tracks {
		if pt.ft.outTrack.conv != nil {
			return false
		}
	}
	return true
}

// table returns window n's table: cached, in flight, or built by one structure-only walk.
func (p *HLSPlan) table(ctx context.Context, n int) (*windowTable, error) {
	p.tabMu.Lock()
	if t := p.tables[n]; t != nil {
		p.tabMu.Unlock()
		return t, nil
	}
	if f := p.tabFlight[n]; f != nil {
		f.join()
		p.tabMu.Unlock()
		select {
		case <-f.done:
			return f.table, f.err
		case <-ctx.Done():
			p.tabMu.Lock()
			f.leave()
			p.tabMu.Unlock()
			return nil, ctx.Err()
		}
	}
	f := &tableFlight{}
	f.flight = newFlight(&p.tabMu, ctx, func() {
		if p.tabFlight[n] == f {
			delete(p.tabFlight, n)
		}
	})
	if p.tabFlight == nil {
		p.tabFlight = make(map[int]*tableFlight)
	}
	p.tabFlight[n] = f
	p.tabMu.Unlock()

	f.table, f.err = p.buildTable(f.ctx, n)

	p.tabMu.Lock()
	if p.tabFlight[n] == f {
		delete(p.tabFlight, n)
	}
	if f.err == nil {
		p.storeTable(n, f.table)
	}
	p.tabMu.Unlock()
	f.finish()
	return f.table, f.err
}

// storeTable keeps a table within the byte budget, oldest out first. Caller holds tabMu.
func (p *HLSPlan) storeTable(n int, t *windowTable) {
	if p.tables == nil {
		p.tables = make(map[int]*windowTable)
	}
	if _, dup := p.tables[n]; dup {
		return
	}
	p.tables[n] = t
	p.tabOrder = append(p.tabOrder, n)
	p.tabBytes += t.bytes
	p.tabAcct.bytes += t.bytes
	tableBytesTotal.Add(t.bytes)
	for (p.tabBytes > tableCacheBytes || tableBytesTotal.Load() > tableTotalBudget.Load()) && len(p.tabOrder) > 0 {
		old := p.tabOrder[0]
		p.tabOrder = p.tabOrder[1:]
		if ot := p.tables[old]; ot != nil {
			p.tabBytes -= ot.bytes
			p.tabAcct.bytes -= ot.bytes
			tableBytesTotal.Add(-ot.bytes)
			delete(p.tables, old)
			p.winMu.Lock()
			p.stats.TableEvictions++
			p.winMu.Unlock()
		}
	}
}

// buildTable walks window n structure-only and times every rendition exactly as buildWindow does.
func (p *HLSPlan) buildTable(ctx context.Context, n int) (*windowTable, error) {
	if n < 0 || n >= p.segCount {
		return nil, errf("segment %d out of range (0..%d)", n, p.segCount-1)
	}
	release, waited, err := acquireWalk(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	segStart := p.bounds[n]
	var segEnd int64 = 1<<63 - 1
	if n+1 < p.segCount {
		segEnd = p.bounds[n+1]
	}
	// Headers only, every track: each sample's bytes are copied from the file at its offset when served.
	windows, nextPts, _, err := p.walkWindow(ctx, n, segStart, segEnd, true, false)
	if err != nil {
		return nil, err
	}
	var otherBytes int64 // what the non-video renditions weigh in this window, for the next estimate
	for ti := range p.tracks {
		if !p.tracks[ti].ft.outTrack.spec.video {
			for _, s := range windows[ti] {
				otherBytes += int64(s.size)
			}
		}
	}
	p.learnOtherRate(n, otherBytes)
	t := &windowTable{segs: make([]trackSegment, len(p.tracks)), heads: make([][]byte, len(p.tracks)), offs: make([][]int64, len(p.tracks))}
	for ti := range p.tracks {
		k, err := p.gridStartFor(ctx, n, ti, windows[ti])
		if err != nil {
			return nil, err
		}
		t.segs[ti] = p.timeWindow(ti, n, windows[ti], nextPts[ti], k)
		t.heads[ti] = buildSegmentFile(uint32(n+1), t.segs[ti])
		t.offs[ti] = make([]int64, len(windows[ti]))
		for x := range windows[ti] {
			t.offs[ti][x] = windows[ti][x].off
		}
		t.bytes += int64(len(t.heads[ti])) + int64(len(windows[ti]))*(8+int64(fragSampleBytes))
	}
	p.winMu.Lock()
	p.stats.TableBuilds++
	if waited {
		p.stats.SlotWaits++
	}
	p.winMu.Unlock()
	return t, nil
}

// mp4Table is segment n of an MP4 source's track ti as a table: the sample table holds its timing and offsets, nothing is walked or kept.
func (p *HLSPlan) mp4Table(ti, n int) (*windowTable, error) {
	if n < 0 || n >= p.segCount {
		return nil, errf("segment %d out of range (0..%d)", n, p.segCount-1)
	}
	t := &windowTable{segs: make([]trackSegment, len(p.tracks)), heads: make([][]byte, len(p.tracks)), offs: make([][]int64, len(p.tracks))}
	t.segs[ti], t.offs[ti] = p.mp4Segment(ti, n)
	t.heads[ti] = buildSegmentFile(uint32(n+1), t.segs[ti])
	return t, nil
}

// fragSampleBytes is a table's cost per sample, for the budget.
const fragSampleBytes = 40

// ResourceHandle is an open resource: its type and exact size up front, its
// bytes written on demand. A media segment of a plan that streams is read from
// the source as it is written, through one pooled buffer, so a client holds a
// buffer, never a segment. Everything else holds its bytes, as Resource does.
type ResourceHandle struct {
	name        string
	contentType string
	size        int64
	data        []byte
	plan        *HLSPlan
	table       *windowTable
	ti          int
	n           int         // the segment's 0-based window
	head        []byte      // the segment's head when it is not the table's (CENC: framed with its senc)
	cenc        *cencStream // a CENC segment's cipher and auxiliary data
}

// Open opens the named resource (the same names Resource serves); the handle's
// bytes equal Resource's exactly. A segment streams when the plan can read its
// source twice: a Matroska or MP4 file, no track conversion, local or
// Options.StreamFromFS. An AES-128 segment is encrypted as it is written; a
// CENC segment's samples are read once by Open, to frame its moof, then again
// as they are encrypted and written, one at a time.
func (p *HLSPlan) Open(ctx context.Context, name string) (*ResourceHandle, error) {
	ti, n, isSeg := p.segmentName(name)
	if !isSeg || !p.streams() || !p.tracks[ti].ft.outTrack.mkv.Compression.Verbatim() {
		data, ct, err := p.Resource(ctx, name)
		if err != nil {
			return nil, err
		}
		if isSeg {
			p.winMu.Lock()
			p.stats.StreamFallbacks++
			p.winMu.Unlock()
		}
		return &ResourceHandle{name: name, contentType: ct, size: int64(len(data)), data: data}, nil
	}
	if err := p.checkSource(); err != nil {
		return nil, err
	}
	var (
		t   *windowTable
		err error
	)
	if p.mp4src {
		if t, err = p.mp4Table(ti, n); err != nil {
			return nil, err
		}
	} else if t, err = p.table(ctx, n); err != nil {
		return nil, err
	}
	h := &ResourceHandle{name: name, contentType: "video/iso.segment", plan: p, table: t, ti: ti, n: n, head: t.heads[ti]}
	if p.opts.CENC != nil {
		if err := p.openCENC(ctx, h); err != nil {
			return nil, err
		}
	}
	h.size = int64(len(h.head)) + t.segs[ti].dataLen
	if p.opts.Encrypt != nil {
		h.size = h.size/aes.BlockSize*aes.BlockSize + aes.BlockSize // PKCS#7 pads to the next block, a whole one when aligned
	}
	p.winMu.Lock()
	p.stats.StreamedSegments++
	p.winMu.Unlock()
	return h, nil
}

// ETag identifies the resource's bytes: strong for bytes in hand, else weak from the size, the source's SourceID and the name.
func (h *ResourceHandle) ETag() string {
	if h.data != nil {
		sum := sha256.Sum256(h.data)
		return `"` + hex.EncodeToString(sum[:]) + `"`
	}
	return fmt.Sprintf("W/\"%x-%s-%s\"", h.size, h.plan.SourceID(), h.name)
}

// SourceID names what the plan's URLs serve - the source's size, mtime and
// identity, the packaging (playlists, init segments, so the options) and the
// mkvgo version, hashed - for a URL prefix or a cache key that changes with
// any of them; empty for a plan without a stamp (a growing source).
func (p *HLSPlan) SourceID() string {
	if p.stamp == "" {
		return ""
	}
	p.sourceIDOnce.Do(func() {
		h := sha256.New()
		fmt.Fprintf(h, "%s|%s|%s|", p.stamp, p.srcID, packagerVersion())
		for _, b := range append(append([][]byte{p.master, p.mpd}, p.medias...), p.inits...) {
			fmt.Fprintf(h, "%d:", len(b))
			h.Write(b)
		}
		p.sourceID = hex.EncodeToString(h.Sum(nil)[:8])
	})
	return p.sourceID
}

// packagerVersion is the mkvgo build that packages: another version may cut other bytes under the same names.
var packagerVersion = sync.OnceValue(func() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	mods := append([]*debug.Module{&bi.Main}, bi.Deps...)
	for _, m := range mods {
		if m.Path == "github.com/gravity-zero/mkvgo" {
			if m.Replace != nil {
				m = m.Replace
			}
			return m.Version + " " + m.Sum
		}
	}
	return ""
})

// segmentName parses a media segment's resource name into its track index and 0-based window.
func (p *HLSPlan) segmentName(name string) (ti, n int, ok bool) {
	fts := p.fts()
	if _, err := fmt.Sscanf(name, "seg%d.m4s", &n); err == nil && name == fmt.Sprintf("seg%05d.m4s", n) {
		return p.videoIndex(), n - 1, true
	}
	var a int
	if _, err := fmt.Sscanf(name, "seg_a%d_%d.m4s", &a, &n); err == nil && a >= 1 {
		for i := range fts {
			if name == renditionSegment(fts, i, n-1) {
				return i, n - 1, true
			}
		}
	}
	return 0, 0, false
}

// ContentType is the resource's MIME type.
func (h *ResourceHandle) ContentType() string { return h.contentType }

// Size is the resource's exact length in bytes.
func (h *ResourceHandle) Size() int64 { return h.size }

// Close releases the handle; nothing is held open between writes.
func (h *ResourceHandle) Close() error { return nil }

// Bytes returns the resource when the handle holds it whole, nil when it streams from the source.
func (h *ResourceHandle) Bytes() []byte { return h.data }

// WriteTo writes the whole resource.
func (h *ResourceHandle) WriteTo(ctx context.Context, w io.Writer) (int64, error) {
	return h.WriteRange(ctx, w, 0, h.size)
}

// WriteRange writes bytes [off, off+n) of the resource. A streamed segment's
// bytes come from the source as they are written; an error midway leaves the
// output short, which the caller must turn into a broken connection, never a
// clean end.
func (h *ResourceHandle) WriteRange(ctx context.Context, w io.Writer, off, n int64) (int64, error) {
	if off < 0 || n < 0 || off > h.size || n > h.size-off {
		return 0, errors.New("mp4: range outside the resource")
	}
	if h.data != nil {
		m, err := w.Write(h.data[off : off+n])
		if err == nil && int64(m) < n {
			err = io.ErrShortWrite
		}
		return int64(m), err
	}
	switch {
	case h.cenc != nil:
		return h.plan.writeCENC(ctx, w, h, off, n)
	case h.plan.opts.Encrypt != nil:
		return h.plan.writeAES(ctx, w, h, off, n)
	}
	return h.plan.writeSpans(ctx, w, h.head, h.table, h.ti, off, n)
}

// writeSpans writes [off, off+n) of track ti's segment in table t: the head
// first, then each sample as its stripped header (if any) and its bytes from
// the source, through one copier that gathers the small spans and hands the
// large ones to the writer straight from the file.
func (p *HLSPlan) writeSpans(ctx context.Context, w io.Writer, head []byte, t *windowTable, ti int, off, n int64) (int64, error) {
	// Audio sits in short runs between video frames: read run by run, through the small frames only.
	c := &spanCopier{exact: !p.tracks[ti].ft.outTrack.spec.video, wipe: p.encrypts()}
	defer c.release()
	// clip is the part [a, b) of a piece spanning [pos, pos+size) that falls in the range.
	clip := func(pos, size int64) (a, b int64, ok bool) {
		from, to := max(off, pos), min(off+n, pos+size)
		return from - pos, to - pos, from < to
	}
	if a, b, ok := clip(0, int64(len(head))); ok {
		if err := c.emit(w, head[a:b]); err != nil {
			return c.written, err
		}
	}
	pos := int64(len(head))
	if pos >= off+n {
		err := c.flush(w)
		return c.written, err
	}
	src, err := p.openSource(ctx)
	if err != nil {
		return c.written, err
	}
	defer src.Close()
	bufp := streamBufs.Get().(*[]byte)
	defer putStreamBuf(bufp, c.wipe)
	c.src, c.buf = src, *bufp
	if f, ok := src.(*os.File); ok {
		c.file = f
	}
	prefix := p.tracks[ti].ft.outTrack.mkv.HeaderStripping
	c.end = spansEnd(src, t, ti, pos, off, off+n, int64(len(prefix)))
	// The pieces in the range, in order: stripped headers to emit, and source
	// runs, samples stored back to back merged into one.
	var pieces []span
	for x, s := range t.segs[ti].samples {
		if pos >= off+n {
			break
		}
		if a, b, ok := clip(pos, int64(len(prefix))); ok {
			pieces = append(pieces, span{emit: prefix[a:b]})
		}
		pos += int64(len(prefix))
		raw := int64(s.size) - int64(len(prefix))
		if a, b, ok := clip(pos, raw); ok {
			o := t.offs[ti][x] + a
			if k := len(pieces) - 1; k >= 0 && pieces[k].emit == nil && pieces[k].off+pieces[k].n == o {
				pieces[k].n += b - a
			} else {
				pieces = append(pieces, span{off: o, n: b - a})
			}
		}
		pos += raw
	}
	for i, pc := range pieces {
		if err := ctx.Err(); err != nil {
			return c.written, err
		}
		if pc.emit != nil {
			if err := c.emit(w, pc.emit); err != nil {
				return c.written, err
			}
			continue
		}
		if c.exact {
			c.reach = exactReach(pieces, i, len(c.buf))
		}
		if err := c.copy(w, pc.off, pc.n); err != nil {
			return c.written, err
		}
	}
	err = c.flush(w)
	return c.written, err
}

// span is one piece of a streamed segment: bytes to emit as they are, or the source's [off, off+n).
type span struct {
	emit   []byte
	off, n int64
}

// exactReadGap is the largest hole an exact copier reads through to take the
// next runs in the same read: a read costs a round trip on a network mount.
var exactReadGap int64 = 64 << 10

// exactReach is how far one read for run i may go: through the runs after it
// while the hole before each is at most exactReadGap and the read fits the
// buffer.
func exactReach(pieces []span, i, bufLen int) int64 {
	start, end := pieces[i].off, pieces[i].off+pieces[i].n
	for _, pc := range pieces[i+1:] {
		if pc.emit != nil {
			continue
		}
		if pc.off < end || pc.off-end > exactReadGap || pc.off+pc.n-start > int64(bufLen) {
			break
		}
		end = pc.off + pc.n
	}
	return end
}

// spansEnd is where in the source the bytes of track ti's samples falling in
// [off, end) of the resource end, the samples starting at pos in it (0 when
// none do). A source that can take it (a remote one) is told where they
// start and end: one request for them instead of a window at a time.
func spansEnd(src io.Reader, t *windowTable, ti int, pos, off, end, prefix int64) int64 {
	from, to := int64(-1), int64(0)
	for x, s := range t.segs[ti].samples {
		if pos >= end {
			break
		}
		next := pos + int64(s.size)
		if max(off, pos+prefix) < min(end, next) {
			if from < 0 {
				from = t.offs[ti][x]
			}
			to = t.offs[ti][x] + int64(s.size) - prefix
		}
		pos = next
	}
	if h, ok := src.(mkv.RangeHinter); ok && from >= 0 {
		h.HintRange(from, to)
	}
	return to
}

// spanCopier copies ranges of a source to a writer: through one buffer,
// reading forward across small holes and seeking over large ones, the small
// spans packed into writes of stageBytes; a span of sendfileMin or more goes
// to a writer that can read from the file itself.
type spanCopier struct {
	src      io.ReadSeeker
	file     *os.File // the source as a file, for the spans the writer takes itself
	buf      []byte
	bufStart int64 // source offset of buf[0]
	bufLen   int   // valid bytes in buf
	pos      int64 // the source's current offset
	seeked   bool
	wipe     bool     // the bytes are an encrypted segment's plaintext: pooled buffers go back zeroed
	exact    bool     // read each span alone, never ahead of it - but up to reach
	reach    int64    // where an exact read may stop at the furthest (0: the span's end)
	end      int64    // where the last span ends in the source: no read goes past it (0: no bound)
	pending  [][]byte // queued, not yet written: slices of buf and of emitted bytes
	pendingN int
	stage    *[]byte
	written  int64
}

// emit queues bytes the caller holds (a head, a stripped header).
func (c *spanCopier) emit(w io.Writer, b []byte) error {
	return c.queue(w, b)
}

// copy queues the source's bytes [off, off+n), or hands them to the writer from the file.
func (c *spanCopier) copy(w io.Writer, off, n int64) error {
	for n > 0 {
		if off >= c.bufStart && off < c.bufStart+int64(c.bufLen) {
			b := c.buf[off-c.bufStart : c.bufLen]
			if int64(len(b)) > n {
				b = b[:n]
			}
			if err := c.queue(w, b); err != nil {
				return err
			}
			off += int64(len(b))
			n -= int64(len(b))
			continue
		}
		if err := c.flush(w); err != nil { // the buffer is about to be refilled, or skipped
			return err
		}
		if c.file != nil && n >= sendfileMin {
			if rf, ok := w.(io.ReaderFrom); ok {
				if _, err := c.file.Seek(off, io.SeekStart); err != nil {
					return err
				}
				m, err := rf.ReadFrom(io.LimitReader(c.file, n))
				c.written += m
				off += m
				n -= m
				c.pos, c.seeked = off, true
				if err != nil {
					return err
				}
				if m == 0 {
					return io.ErrUnexpectedEOF
				}
				continue
			}
		}
		if c.exact || !c.seeked || off < c.pos || off-c.pos > spanMergeGap {
			if _, err := c.src.Seek(off, io.SeekStart); err != nil {
				return err
			}
			c.pos, c.seeked = off, true
		}
		want := c.buf
		if lim := max(n, c.reach-off); c.exact && lim < int64(len(want)) {
			want = want[:lim]
		}
		if rest := c.end - c.pos; rest >= n && rest < int64(len(want)) { // never read past the last span
			want = want[:rest]
		}
		m, err := io.ReadFull(c.src, want)
		if m == 0 {
			if err == nil || errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF // the source ends before the sample does
			}
			return err
		}
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return err
		}
		c.bufStart, c.bufLen = c.pos, m
		c.pos += int64(m)
	}
	return nil
}

// queue adds b to the pending writes, flushing when they reach the stage size.
func (c *spanCopier) queue(w io.Writer, b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if len(b) >= stageBytes { // a stage's worth on its own: packing it would only copy it
		if err := c.flush(w); err != nil {
			return err
		}
		return c.write(w, b)
	}
	c.pending = append(c.pending, b)
	c.pendingN += len(b)
	if c.pendingN >= stageBytes {
		return c.flush(w)
	}
	return nil
}

// flush writes what is pending in one write, packed through the stage when it is several pieces.
func (c *spanCopier) flush(w io.Writer) error {
	if len(c.pending) == 0 {
		return nil
	}
	pending := c.pending
	c.pending, c.pendingN = c.pending[:0], 0
	if len(pending) == 1 {
		return c.write(w, pending[0])
	}
	if c.stage == nil {
		c.stage = stageBufs.Get().(*[]byte)
	}
	st := (*c.stage)[:0]
	for _, b := range pending {
		st = append(st, b...)
	}
	*c.stage = st[:0]
	return c.write(w, st)
}

// write hands b to the writer; fewer bytes taken without an error is io.ErrShortWrite.
func (c *spanCopier) write(w io.Writer, b []byte) error {
	m, err := w.Write(b)
	c.written += int64(m)
	if err == nil && m < len(b) {
		err = io.ErrShortWrite
	}
	return err
}

func (c *spanCopier) release() {
	if c.stage != nil {
		if c.wipe {
			clear((*c.stage)[:cap(*c.stage)])
		}
		stageBufs.Put(c.stage)
		c.stage = nil
	}
}

// putStreamBuf returns a copier buffer to its pool, zeroed when it held an encrypted segment's plaintext.
func putStreamBuf(bufp *[]byte, wipe bool) {
	if wipe {
		clear(*bufp)
	}
	streamBufs.Put(bufp)
}

// encrypts reports whether the plan's segments are encrypted (AES-128 or CENC).
func (p *HLSPlan) encrypts() bool { return p.opts.Encrypt != nil || p.opts.CENC != nil }

// ErrSourceChanged is returned for a media or subtitle segment once the source
// is not the file the plan was built on (its size, mtime or identity moved):
// the plan's offsets belong to another file. Build a new plan.
var ErrSourceChanged = mkv.ErrSourceChanged

// checkSource refuses the source once its stamp or identity moved. Not run for a plan without a stamp (growing source, or one it could not stat at birth), nor for a remote source whose reads are pinned: they verify it themselves, a stat there is a round trip for nothing.
func (p *HLSPlan) checkSource() error {
	if p.srcChanged.Load() {
		return ErrSourceChanged
	}
	if p.stamp == "" || strings.HasPrefix(p.stamp, "t") || (p.pinned && !p.osStat) {
		return nil
	}
	// An OS file is fstat'ed on every read: the path stat only fails a request before its first byte, once per interval is enough.
	now := int64(time.Since(clockBase)) + 1 // monotonic: the wall clock steps back (a VM resyncing it)
	if at := p.srcStatAt.Load(); p.pinned && at != 0 && now-at < sourceStatEvery.Load() {
		return nil
	}
	fi, err := p.fs.DoStat(p.srcPath)
	if err != nil {
		return err
	}
	id := p.srcID
	if fid, ok := fi.(mkv.FileIdentity); ok {
		id = fid.Identity()
	} else if p.osStat {
		id = fileid.Of(fi)
	}
	if statStamp(fi) != p.stamp || id != p.srcID || (p.osStat && !os.SameFile(p.srcInfo, fi)) {
		p.srcChanged.Store(true)
		return ErrSourceChanged
	}
	p.srcStatAt.Store(now)
	return nil
}

// sourceStatEvery spaces the path stat of a source whose reads verify it themselves (ns): a stat is a round trip on a network mount.
var sourceStatEvery atomic.Int64

func init() { sourceStatEvery.Store(int64(time.Second)) }

// clockBase anchors the monotonic times srcStatAt holds.
var clockBase = time.Now()
