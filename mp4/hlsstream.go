package mp4

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
)

// tableCacheBytes bounds the window tables one plan keeps: a table is the video's structure plus the window's audio segments, a few MiB at most.
const tableCacheBytes = 16 << 20

// audioInTableBytes is the most a window's non-video payloads may weigh for the
// structure walk to keep them and serve those renditions from the table; a
// heavier window has them written from the source by ranges, like the video.
var audioInTableBytes int64 = 2 << 20

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

// audioExactReads has a non-video rendition's copier read each payload exactly
// - a seek and a read per sample, never through the holes - instead of
// reading forward through the video frames between them.
var audioExactReads = false

// streamBufBytes is the copier's buffer: what a streamed segment costs a client.
const streamBufBytes = 256 << 10

var streamBufs = sync.Pool{New: func() any { b := make([]byte, streamBufBytes); return &b }}

// windowTable is one window's structure: every rendition timed, its segment
// head framed, and where each sample's bytes sit in the source - enough to
// write any of its segments from the source without holding one.
type windowTable struct {
	segs  []trackSegment
	heads [][]byte
	offs  [][]int64
	data  [][]byte // a non-video rendition's whole segment, built by the structure walk that kept its payloads
	bytes int64
}

type tableFlight struct {
	done  chan struct{}
	table *windowTable
	err   error
}

// streams reports whether this plan can write segments from the source: the
// served bytes must be the source's own (no encryption, no conversion), from a
// Matroska file the plan may read twice.
func (p *HLSPlan) streams() bool {
	if p.mp4src || p.opts.Encrypt != nil || p.opts.CENC != nil || (p.opts.FS != nil && !p.opts.StreamFromFS) {
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
		p.tabMu.Unlock()
		select {
		case <-f.done:
			return f.table, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &tableFlight{done: make(chan struct{})}
	if p.tabFlight == nil {
		p.tabFlight = make(map[int]*tableFlight)
	}
	p.tabFlight[n] = f
	p.tabMu.Unlock()

	f.table, f.err = p.buildTable(ctx, n)

	p.tabMu.Lock()
	delete(p.tabFlight, n)
	if f.err == nil {
		p.storeTable(n, f.table)
	}
	p.tabMu.Unlock()
	close(f.done)
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
	// A light window's audio rides in the table; a heavy one (lossless, many
	// tracks) is streamed like the video. Unknown yet means light: most are.
	other := p.otherEstimate(n)
	keepAudio := other <= audioInTableBytes
	windows, nextPts, _, err := p.walkWindow(ctx, n, segStart, segEnd, true, keepAudio)
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
	t := &windowTable{segs: make([]trackSegment, len(p.tracks)), heads: make([][]byte, len(p.tracks)), offs: make([][]int64, len(p.tracks)), data: make([][]byte, len(p.tracks))}
	for ti := range p.tracks {
		k, err := p.gridStartFor(ctx, n, ti, windows[ti])
		if err != nil {
			return nil, err
		}
		if keepAudio && !p.tracks[ti].ft.outTrack.spec.video {
			// The walk kept this rendition's payloads: its segment is built
			// now, once, and served whole - a second pass over the window to
			// pick its blocks out from between the video's would read it all again.
			data, err := p.buildTrackSegment(ti, n, windows[ti], nextPts[ti], k, nil)
			if err != nil {
				return nil, err
			}
			t.data[ti] = data
			t.bytes += int64(len(data))
			continue
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
}

// Open opens the named resource (the same names Resource serves); the handle's
// bytes equal Resource's exactly. A segment streams when the plan can read its
// source twice and serves the source's own bytes: a Matroska file, no
// encryption, no track conversion, local or Options.StreamFromFS.
func (p *HLSPlan) Open(ctx context.Context, name string) (*ResourceHandle, error) {
	ti, n, isSeg := p.segmentName(name)
	if !isSeg || !p.streams() || (p.tracks[ti].ft.outTrack.spec.video && !p.tracks[ti].ft.outTrack.mkv.Compression.Verbatim()) {
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
	t, err := p.table(ctx, n)
	if err != nil {
		return nil, err
	}
	if d := t.data[ti]; d != nil { // an audio rendition: built whole by the structure walk
		return &ResourceHandle{name: name, contentType: "video/iso.segment", size: int64(len(d)), data: d, plan: p}, nil
	}
	p.winMu.Lock()
	p.stats.StreamedSegments++
	p.winMu.Unlock()
	return &ResourceHandle{name: name, contentType: "video/iso.segment", size: int64(len(t.heads[ti])) + t.segs[ti].dataLen, plan: p, table: t, ti: ti}, nil
}

// ETag identifies the resource's bytes: strong for bytes in hand, else weak from the size, the source's stamp and the name.
func (h *ResourceHandle) ETag() string {
	if h.data != nil {
		sum := sha256.Sum256(h.data)
		return `"` + hex.EncodeToString(sum[:]) + `"`
	}
	return fmt.Sprintf("W/\"%x-%s-%s\"", h.size, h.plan.stamp, h.name)
}

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
	if off < 0 || n < 0 || off+n > h.size {
		return 0, errors.New("mp4: range outside the resource")
	}
	if h.data != nil {
		m, err := w.Write(h.data[off : off+n])
		return int64(m), err
	}
	return h.plan.writeSpans(ctx, w, h.table, h.ti, off, n)
}

// writeSpans writes [off, off+n) of track ti's segment in table t: the head
// first, then each sample as its stripped header (if any) and its bytes from
// the source, consecutive samples read through one buffer.
func (p *HLSPlan) writeSpans(ctx context.Context, w io.Writer, t *windowTable, ti int, off, n int64) (int64, error) {
	var written int64
	emit := func(b []byte) error {
		m, err := w.Write(b)
		written += int64(m)
		return err
	}
	// cut writes the part of a piece spanning [pos, pos+size) that falls in the range, through fn.
	cut := func(pos, size int64, fn func(from, to int64) error) error {
		from, to := max(off, pos), min(off+n, pos+size)
		if from >= to {
			return nil
		}
		return fn(from-pos, to-pos)
	}
	head := t.heads[ti]
	if err := cut(0, int64(len(head)), func(a, b int64) error { return emit(head[a:b]) }); err != nil {
		return written, err
	}
	pos := int64(len(head))
	if pos >= off+n {
		return written, nil
	}
	src, err := p.fs.DoOpen(p.srcPath)
	if err != nil {
		return written, err
	}
	defer src.Close()
	bufp := streamBufs.Get().(*[]byte)
	defer streamBufs.Put(bufp)
	c := spanCopier{src: src, buf: *bufp, exact: audioExactReads && !p.tracks[ti].ft.outTrack.spec.video}
	prefix := p.tracks[ti].ft.outTrack.mkv.HeaderStripping
	for x, s := range t.segs[ti].samples {
		if pos >= off+n {
			break
		}
		if err := ctx.Err(); err != nil {
			return written, err
		}
		if err := cut(pos, int64(len(prefix)), func(a, b int64) error { return emit(prefix[a:b]) }); err != nil {
			return written, err
		}
		pos += int64(len(prefix))
		raw := int64(s.size) - int64(len(prefix))
		err := cut(pos, raw, func(a, b int64) error {
			m, err := c.copy(w, t.offs[ti][x]+a, b-a)
			written += m
			return err
		})
		if err != nil {
			return written, err
		}
		pos += raw
	}
	return written, nil
}

// spanCopier copies ranges of a source to a writer through one buffer, reading
// forward across small holes and seeking over large ones.
type spanCopier struct {
	src      io.ReadSeeker
	buf      []byte
	bufStart int64 // source offset of buf[0]
	bufLen   int   // valid bytes in buf
	pos      int64 // the source's current offset
	seeked   bool
	exact    bool // read each span alone, never ahead of it
}

// copy writes the source's bytes [off, off+n) to w.
func (c *spanCopier) copy(w io.Writer, off, n int64) (int64, error) {
	var written int64
	for n > 0 {
		if off >= c.bufStart && off < c.bufStart+int64(c.bufLen) {
			b := c.buf[off-c.bufStart : c.bufLen]
			if int64(len(b)) > n {
				b = b[:n]
			}
			m, err := w.Write(b)
			written += int64(m)
			if err != nil {
				return written, err
			}
			off += int64(m)
			n -= int64(m)
			continue
		}
		if c.exact || !c.seeked || off < c.pos || off-c.pos > spanMergeGap {
			if _, err := c.src.Seek(off, io.SeekStart); err != nil {
				return written, err
			}
			c.pos, c.seeked = off, true
		}
		want := c.buf
		if c.exact && n < int64(len(want)) {
			want = want[:n]
		}
		m, err := io.ReadFull(c.src, want)
		if m == 0 {
			if err == nil || errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF // the source ends before the sample does
			}
			return written, err
		}
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return written, err
		}
		c.bufStart, c.bufLen = c.pos, m
		c.pos += int64(m)
	}
	return written, nil
}
