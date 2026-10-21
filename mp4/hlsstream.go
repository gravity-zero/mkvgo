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
	"weak"

	"github.com/gravity-zero/mkvgo/internal/fileid"
	"github.com/gravity-zero/mkvgo/internal/poison"
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

// tableAccount is a plan's share of tableBytesTotal and of windowBytesTotal, given back when the plan is collected.
type tableAccount struct{ bytes, held int64 }

func newTableAccount(p *HLSPlan) *tableAccount {
	a := &tableAccount{}
	runtime.AddCleanup(p, func(a *tableAccount) {
		tableBytesTotal.Add(-a.bytes)
		windowBytesTotal.Add(-a.held)
	}, a)
	return a
}

// windowBytesBudget bounds, process-wide, the kept video segments (SetWindowBytes); windowBytesTotal is what tables and handles hold now.
var (
	windowBytesBudget atomic.Int64
	windowBytesTotal  atomic.Int64
)

func init() { windowBytesBudget.Store(defaultWindowBytes) }

// defaultWindowBytes is SetWindowBytes' default: 64 MiB served a network source's windows as 256 did (origin bytes and requests), for a peak a quarter as high.
const defaultWindowBytes = 64 << 20

// SetWindowBytes caps, process-wide, the video segments Open keeps from the walks that built them (held until written
// whole or closed) rather than copying them from the source again; zero restores the default of 64 MiB, negative keeps nothing.
// Only a network source keeps (a Matroska walk's segments, a remote MP4 window's audio): a local file's copy is read
// from the page cache. The budget bounds the bytes kept, not the process's peak: dropped windows wait for the collector,
// which can take the peak to about twice the budget over the rest (GOMEMLIMIT bounds it).
func SetWindowBytes(total int64) {
	switch {
	case total == 0:
		total = defaultWindowBytes
	case total < 0:
		total = 0
	}
	windowBytesBudget.Store(total)
}

// reserveWindowBytes takes n bytes of the window budget, or nothing when they do not fit.
func reserveWindowBytes(n int64) bool {
	for {
		cur := windowBytesTotal.Load()
		if n <= 0 || cur+n > windowBytesBudget.Load() {
			return false
		}
		if windowBytesTotal.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

// heldBytes is a video segment's share of the window budget, given back once: by its handle, or when the handle is collected.
type heldBytes struct{ n atomic.Int64 }

func (h *heldBytes) release() {
	if h != nil {
		if n := h.n.Swap(0); n > 0 {
			windowBytesTotal.Add(-n)
		}
	}
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
	data  [][]byte // a segment built whole by the walk that read it: kept until Open takes it, or a network source's light audio
	// packed holds a cached table's renditions' samples (see tablecodec.go),
	// unpacked into segs and offs per request, for the one served (view).
	packed [][]byte
	bytes  int64
	kept   []int64 // per track, what data holds under the window budget until taken; their sum is held
	taken  []bool  // per track, whether a kept segment was handed out
	held   int64
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

// takeKept hands out a segment window n's table kept from its walk, once: the table lets it go, the handle holds its budget share.
func (p *HLSPlan) takeKept(t *windowTable, ti, n int) ([]byte, *heldBytes) {
	p.tabMu.Lock()
	defer p.tabMu.Unlock()
	if t.kept == nil || t.kept[ti] == 0 {
		return nil, nil
	}
	d := t.data[ti]
	t.data[ti] = nil
	t.held -= t.kept[ti]
	if p.tables[n] == t {
		p.tabAcct.held -= t.kept[ti]
	}
	h := &heldBytes{}
	h.n.Store(t.kept[ti])
	t.kept[ti] = 0
	t.taken[ti] = true
	if p.keptConsumed(t) { // a viewer took its video and one audio: the other languages are nobody's
		p.dropKept(t)
	}
	return d, h
}

// keptConsumed reports whether a table's viewer has taken what a viewer takes: every kept video and one kept audio.
func (p *HLSPlan) keptConsumed(t *windowTable) bool {
	audioKept, audioTaken := false, false
	for ti := range t.kept {
		video := p.tracks[ti].ft.outTrack.spec.video
		if video && t.kept[ti] > 0 {
			return false
		}
		if !video && (t.kept[ti] > 0 || t.taken[ti]) {
			audioKept = true
			audioTaken = audioTaken || t.taken[ti]
		}
	}
	return !audioKept || audioTaken
}

// dropKept gives back the segments an evicted table still held. Caller holds tabMu.
func (p *HLSPlan) dropKept(t *windowTable) {
	if t.held == 0 {
		return
	}
	for ti := range t.kept {
		if t.kept[ti] > 0 {
			t.data[ti], t.kept[ti] = nil, 0
		}
	}
	p.tabAcct.held -= t.held
	windowBytesTotal.Add(-t.held)
	t.held = 0
}

// keepers are the plans whose tables may hold kept segments, weakly: a plan nobody uses is still collected.
var keepers struct {
	sync.Mutex
	plans []weak.Pointer[HLSPlan]
	known map[weak.Pointer[HLSPlan]]bool
}

// registerKeeper lists p among the plans a reclaim may take kept segments from.
func registerKeeper(p *HLSPlan) {
	w := weak.Make(p)
	keepers.Lock()
	defer keepers.Unlock()
	if keepers.known == nil {
		keepers.known = map[weak.Pointer[HLSPlan]]bool{}
	}
	if !keepers.known[w] {
		keepers.known[w] = true
		keepers.plans = append(keepers.plans, w)
	}
}

// reclaimKept gives back kept segments nobody collected, this plan's oldest tables first, then the other plans',
// until need fits the process budget; it reports whether it gave anything back. A segment reclaimed is copied
// from the source when asked for, as one the budget never let a walk keep.
func (p *HLSPlan) reclaimKept(need int64) bool {
	freed := p.dropOldestKept(need)
	keepers.Lock()
	others := make([]*HLSPlan, 0, len(keepers.plans))
	live := keepers.plans[:0]
	for _, w := range keepers.plans {
		if q := w.Value(); q != nil {
			live = append(live, w)
			if q != p {
				others = append(others, q)
			}
		} else {
			delete(keepers.known, w)
		}
	}
	keepers.plans = live
	keepers.Unlock()
	for _, q := range others {
		if windowBytesTotal.Load()+need <= windowBytesBudget.Load() {
			break
		}
		freed = q.dropOldestKept(need) || freed
	}
	return freed
}

// dropOldestKept gives back the kept segments of p's oldest tables until need fits the process budget.
func (p *HLSPlan) dropOldestKept(need int64) bool {
	p.tabMu.Lock()
	defer p.tabMu.Unlock()
	freed := false
	for _, n := range p.tabOrder {
		if windowBytesTotal.Load()+need <= windowBytesBudget.Load() {
			break
		}
		if t := p.tables[n]; t != nil && t.held > 0 {
			p.dropKept(t)
			freed = true
		}
	}
	if windowBytesTotal.Load()+need > windowBytesBudget.Load() {
		freed = p.share.reclaim(need) || freed // a remote MP4's kept audio
	}
	return freed
}

// storeTable keeps a table within the byte budget, oldest out first. Caller holds tabMu.
func (p *HLSPlan) storeTable(n int, t *windowTable) {
	if p.tables == nil {
		p.tables = make(map[int]*windowTable)
	}
	if _, dup := p.tables[n]; dup {
		for ti := range t.kept { // the copy not stored: its waiters copy from the source, its budget is given back
			if t.kept[ti] > 0 {
				t.data[ti], t.kept[ti] = nil, 0
			}
		}
		windowBytesTotal.Add(-t.held)
		t.held = 0
		return
	}
	p.tables[n] = t
	p.tabOrder = append(p.tabOrder, n)
	p.tabBytes += t.bytes
	p.tabAcct.bytes += t.bytes
	tableBytesTotal.Add(t.bytes)
	p.tabAcct.held += t.held
	if t.held > 0 {
		registerKeeper(p)
	}
	for (p.tabBytes > tableCacheBytes || tableBytesTotal.Load() > tableTotalBudget.Load()) && len(p.tabOrder) > 0 {
		old := p.tabOrder[0]
		p.tabOrder = p.tabOrder[1:]
		if ot := p.tables[old]; ot != nil {
			p.tabBytes -= ot.bytes
			p.tabAcct.bytes -= ot.bytes
			tableBytesTotal.Add(-ot.bytes)
			p.dropKept(ot)
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
		return nil, noResource("segment %d out of range (0..%d)", n, p.segCount-1)
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
	// The walk reads the whole window whatever it keeps (every payload sits
	// under the reader's seek threshold): a network source's walk keeps,
	// under the window budget, the segments it read - the video, each light
	// audio - for the Opens that take them, so serving the window reads the
	// source once. A local file's copy comes from the page cache the walk
	// just filled. Otherwise headers only: each sample's bytes are copied from the file at
	// its offset when served, and only a network source's light audio rides
	// in the table (read with the walk, it costs no round trip of its own).
	est := p.windowSpan(n)
	keepable := est > 0 && est <= maxWindowInPlace && p.networkSrc(ctx) && windowBytesBudget.Load() > 0 // the window keep applies here
	keep := keepable && (reserveWindowBytes(est) || p.reclaimKept(est) && reserveWindowBytes(est))
	keepAudio := !keep && p.otherEstimate(n) <= audioInTableBytes && p.networkSrc(ctx)
	windows, nextPts, inPlace, err := p.walkWindow(ctx, n, segStart, segEnd, !keep, keepAudio)
	if err != nil {
		if keep {
			windowBytesTotal.Add(-est)
		}
		p.noteDamageRefused(err)
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
	t := &windowTable{packed: make([][]byte, len(p.tracks)), heads: make([][]byte, len(p.tracks)), data: make([][]byte, len(p.tracks))}
	if keep {
		t.kept, t.taken = make([]int64, len(p.tracks)), make([]bool, len(p.tracks))
	}
	for ti := range p.tracks {
		k, err := p.gridStartFor(ctx, n, ti, windows[ti])
		if err != nil {
			if keep {
				windowBytesTotal.Add(-est)
			}
			return nil, err
		}
		video := p.tracks[ti].ft.outTrack.spec.video
		var payload int64
		for _, s := range windows[ti] {
			payload += int64(s.size)
		}
		if (keep && (video || payload <= audioInTableBytes)) || (keepAudio && !video) {
			var arena []byte
			if ti == inPlace.track && inPlace.media() != nil {
				arena = inPlace.buf
			}
			data, err := p.buildTrackSegment(ti, n, windows[ti], nextPts[ti], k, arena)
			if err != nil {
				if keep {
					windowBytesTotal.Add(-est)
				}
				return nil, err
			}
			t.data[ti] = data
			if keep {
				t.kept[ti] = int64(len(data))
				t.held += t.kept[ti]
			} else {
				t.bytes += int64(len(data))
				continue
			}
		}
		seg := p.timeWindow(ti, n, windows[ti], nextPts[ti], k)
		offs := make([]int64, len(windows[ti]))
		for x := range windows[ti] {
			offs[x] = windows[ti][x].off
		}
		t.packed[ti] = packTrack(seg, offs)
		t.heads[ti] = buildSegmentFile(uint32(n+1), seg)
		t.bytes += int64(cap(t.packed[ti])+cap(t.heads[ti])) + tableTrackOverhead
	}
	if keep {
		windowBytesTotal.Add(t.held - est) // the estimate was the window's span; what is held is the segments
	}
	p.winMu.Lock()
	p.stats.TableBuilds++
	if waited {
		p.stats.SlotWaits++
	}
	if keepable && !keep { // the budget refused: a local source or a keep turned off is no fallback
		p.stats.KeptFallbacks++
	}
	if inPlace.buf != nil && inPlace.media() == nil {
		p.stats.ArenaFallbacks++
	}
	p.winMu.Unlock()
	return t, nil
}

// audioInTableBytes is the most a window may keep of one audio rendition, or (headers-only walk) a network source may hold of
// non-video payloads for its walk to keep them in the table.
var audioInTableBytes int64 = 2 << 20

// networkSrc reports, once per plan, whether the source sits behind a network:
// a file on a network mount, or a source that is no local file at all.
func (p *HLSPlan) networkSrc(ctx context.Context) bool {
	switch p.netSrc.Load() {
	case 1:
		return false
	case 2:
		return true
	}
	switch p.opts.SourceAccess {
	case SourceLocal:
		return false
	case SourceNetwork:
		return true
	}
	src, err := p.openSource(ctx)
	if err != nil {
		return true // not decided: asked again next time
	}
	defer src.Close()
	net := p.onNetwork(src)
	p.netSrc.Store(map[bool]int32{false: 1, true: 2}[net])
	return net
}

// onNetwork says whether reads of the opened source src go across a network:
// Options.SourceAccess when set, else what the file's filesystem is (a source
// that is no local file is a network one).
func (p *HLSPlan) onNetwork(src io.Reader) bool {
	switch p.opts.SourceAccess {
	case SourceLocal:
		return false
	case SourceNetwork:
		return true
	}
	f, ok := src.(*os.File)
	return !ok || networkSource(f)
}

// mp4Table is segment n of an MP4 source's track ti as a table: the sample table holds its timing and offsets, nothing is walked or kept.
func (p *HLSPlan) mp4Table(ti, n int) (*windowTable, error) {
	if n < 0 || n >= p.segCount {
		return nil, noResource("segment %d out of range (0..%d)", n, p.segCount-1)
	}
	t := &windowTable{segs: make([]trackSegment, len(p.tracks)), heads: make([][]byte, len(p.tracks)), offs: make([][]int64, len(p.tracks)), data: make([][]byte, len(p.tracks))}
	t.segs[ti], t.offs[ti] = p.mp4Segment(ti, n)
	t.heads[ti] = buildSegmentFile(uint32(n+1), t.segs[ti])
	return t, nil
}

// tableTrackOverhead is what a cached table costs per rendition besides its packed samples (headers of the slices), for the budget.
const tableTrackOverhead = 64

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
	n           int           // the segment's 0-based window
	head        []byte        // the segment's head when it is not the table's (CENC: framed with its senc)
	cenc        *cencStream   // a CENC segment's cipher and auxiliary data
	held        *heldBytes    // the window budget a kept video segment holds until written whole or closed
	served      *atomic.Int64 // the plan's StreamedBytes, nil for a handle Resource counts
	taken       bool          // Bytes handed the resource out whole: counted served then, never again by a write
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
			return &ResourceHandle{name: name, contentType: ct, size: int64(len(data)), data: data}, nil // counted in ServedBytes
		}
		return &ResourceHandle{name: name, contentType: ct, size: int64(len(data)), data: data, served: &p.streamed}, nil
	}
	if err := p.checkSource(); err != nil {
		return nil, err
	}
	var (
		t   *windowTable
		err error
	)
	if p.mp4src {
		t, err = p.mp4Table(ti, n)
	} else if t, err = p.table(ctx, n); err == nil {
		if d, held := p.takeKept(t, ti, n); d != nil { // a segment the walk kept: this viewer's, once
			p.winMu.Lock()
			p.stats.KeptSegments++
			p.stats.ServedSegments++
			p.winMu.Unlock()
			h := &ResourceHandle{name: name, contentType: "video/iso.segment", size: int64(len(d)), data: d, plan: p, held: held, served: &p.streamed}
			runtime.AddCleanup(h, (*heldBytes).release, held)
			return h, nil
		}
		t = t.view(ti)
	}
	if err != nil {
		return nil, err
	}
	if d := t.data[ti]; d != nil { // a network source's light audio: built whole by the walk, kept in the table
		p.winMu.Lock()
		p.stats.KeptSegments++
		p.stats.ServedSegments++
		p.winMu.Unlock()
		return &ResourceHandle{name: name, contentType: "video/iso.segment", size: int64(len(d)), data: d, plan: p, served: &p.streamed}, nil
	}
	h := &ResourceHandle{name: name, contentType: "video/iso.segment", plan: p, table: t, ti: ti, n: n, head: t.heads[ti], served: &p.streamed}
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
	p.stats.ServedSegments++
	p.winMu.Unlock()
	return h, nil
}

// Stat says what Open would hand out under name - its size, ETag and content type - without handing it out: no
// segment is counted served, none kept is taken, so a HEAD or a revalidation costs the plan nothing. ok is false
// where only Open can tell: a segment that does not stream, a CENC one, whose head Open frames from the samples, or a
// network source's light audio, handed out whole.
func (p *HLSPlan) Stat(ctx context.Context, name string) (size int64, etag, contentType string, ok bool, err error) {
	ti, n, isSeg := p.segmentName(name)
	if !isSeg {
		// A playlist, an init or a subtitle: Open hands it out whole under its bytes' own ETag, counted as streamed.
		data, ct, err := p.Resource(ctx, name)
		if err != nil {
			return 0, "", "", false, err
		}
		sum := sha256.Sum256(data)
		return int64(len(data)), `"` + hex.EncodeToString(sum[:]) + `"`, ct, true, nil
	}
	if !p.streams() || !p.tracks[ti].ft.outTrack.mkv.Compression.Verbatim() || p.opts.CENC != nil {
		return 0, "", "", false, nil
	}
	if err := p.checkSource(); err != nil {
		return 0, "", "", false, err
	}
	var t *windowTable
	if p.mp4src {
		t, err = p.mp4Table(ti, n)
	} else if t, err = p.table(ctx, n); err == nil {
		p.tabMu.Lock()
		kept := t.kept != nil && t.kept[ti] > 0
		size = int64(len(t.data[ti]))
		p.tabMu.Unlock()
		if !kept {
			t, size = t.view(ti), 0
		}
	}
	if err != nil {
		return 0, "", "", false, err
	}
	if size == 0 {
		if t.data[ti] != nil { // handed out whole by Open, under its bytes' own ETag
			return 0, "", "", false, nil
		}
		size = int64(len(t.heads[ti])) + t.segs[ti].dataLen
		if p.opts.Encrypt != nil {
			size = size/aes.BlockSize*aes.BlockSize + aes.BlockSize
		}
	}
	h := ResourceHandle{name: name, size: size, plan: p}
	return size, h.ETag(), "video/iso.segment", true, nil
}

// ETag identifies the resource's bytes: strong for a resource held whole, else weak from the size, the source's SourceID and the name.
func (h *ResourceHandle) ETag() string {
	if h.Whole() {
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

// Close releases the handle: a kept segment gives its window budget back; nothing is held open between writes.
func (h *ResourceHandle) Close() error {
	h.held.release()
	return nil
}

// Whole reports whether the handle holds the resource whole, which Bytes then hands out; it counts nothing served.
func (h *ResourceHandle) Whole() bool { return h.data != nil && h.held == nil }

// Bytes returns the resource when the handle holds it whole and unbudgeted; nil when it streams from the source or
// holds a kept video segment, which is written through the handle and given back by Close. Bytes handed out are
// counted served whole (HLSPlanStats.StreamedBytes), once: the plan cannot see what the caller writes of them.
func (h *ResourceHandle) Bytes() []byte {
	if !h.Whole() {
		return nil
	}
	if h.served != nil && !h.taken {
		h.served.Add(int64(len(h.data)))
	}
	h.taken = true
	return h.data
}

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
	m, err := h.writeRange(ctx, w, off, n)
	if h.served != nil && !h.taken { // what reached w: a range its part, a write cut short what it wrote
		h.served.Add(m)
	}
	return m, err
}

// writeRange writes [off, off+n) from the handle's bytes or through the plan.
func (h *ResourceHandle) writeRange(ctx context.Context, w io.Writer, off, n int64) (int64, error) {
	if h.data != nil {
		m, err := w.Write(h.data[off : off+n])
		if err == nil && int64(m) < n {
			err = io.ErrShortWrite
		}
		if off+n == h.size {
			h.held.release()
		}
		return int64(m), err
	}
	switch {
	case h.cenc != nil:
		return h.plan.writeCENC(ctx, w, h, off, n)
	case h.plan.opts.Encrypt != nil:
		return h.plan.writeAES(ctx, w, h, off, n)
	}
	return h.plan.writeSpans(ctx, w, h.head, h.table, h.ti, h.n, off, n)
}

// writeSpans writes [off, off+n) of track ti's segment in table t: the head
// first, then each sample as its stripped header (if any) and its bytes from
// the source, through one copier that gathers the small spans and hands the
// large ones to the writer straight from the file.
func (p *HLSPlan) writeSpans(ctx context.Context, w io.Writer, head []byte, t *windowTable, ti, win int, off, n int64) (int64, error) {
	// Audio sits in short runs between video frames: read run by run, through the small frames only.
	c := &spanCopier{exact: !p.tracks[ti].ft.outTrack.spec.video, wipe: p.encrypts(), read: &p.copyRead}
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
	if p.shareable(src) {
		// A whole video segment reads its window's audio with it; an audio segment takes what that read kept.
		whole := off == 0 && off+n >= int64(len(head))+t.segs[ti].dataLen
		if shared, err := p.shareWindow(ctx, w, c, src, pieces, win, ti, whole); shared || err != nil {
			return c.written, err
		}
	}
	if ra, ok := src.(io.ReaderAt); ok && c.file == nil && c.exact && p.onNetwork(src) && rangeCount(pieces, remoteReadGap) <= remoteMaxRanges {
		// A remote source answers a range exactly: only the samples' runs are asked for, those closer than remoteReadGap
		// together, when that takes few requests; else one request reads through the holes, each a round trip saved.
		err := c.copyParallel(ctx, w, pieces, ra, remoteReadGap, parallelMaxBytes, remoteReads)
		return c.written, err
	}
	var dense *ahead // a dense rendition read ahead of its copy, nil otherwise
	if c.file != nil {
		if c.exact && p.onNetwork(c.file) {
			// Each read is a round trip on a network mount: the reads go out together.
			err := c.copyParallel(ctx, w, pieces, c.file, exactReadGap, int64(len(c.buf)), parallelReads)
			return c.written, err
		}
		if !p.onNetwork(c.file) {
			dense = prefetch(c.file, pieces, c.exact, len(c.buf))
		}
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
		dense.advance(pc.off)
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

// adviseWillNeed is willNeed, swappable for a test.
var adviseWillNeed = willNeed

// prefetchAhead is how far ahead of its copy a dense rendition is asked for:
// measured against its whole range at once, a cold 1080p video segment came
// 18 % sooner from a fast disk, and as soon from one capped at 250 MB/s.
var prefetchAhead int64 = 8 << 20

// ahead asks for a dense rendition's bytes prefetchAhead in front of its copy.
type ahead struct {
	f         *os.File
	next, end int64 // asked for up to next; the rendition ends at end
}

// advance tops the request up once the copy at at has used half of what was asked.
func (a *ahead) advance(at int64) {
	if a == nil || a.next >= a.end || a.next-at >= prefetchAhead/2 {
		return
	}
	from, to := max(a.next, at), min(a.end, at+prefetchAhead)
	adviseWillNeed(a.f, from, to-from)
	a.next = to
}

// prefetch has the kernel start reading the pieces' source bytes before the
// copy asks for them: a dense rendition (video) prefetchAhead in front of the
// copy, which advances it (on a cold disk, sendfile calls of a frame each
// would otherwise wait for their own small reads), an exact one (audio) read
// by read, all at once.
func prefetch(f *os.File, pieces []span, exact bool, bufLen int) *ahead {
	first, last := -1, -1
	for i, pc := range pieces {
		if pc.emit == nil {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return nil
	}
	if !exact {
		a := &ahead{f: f, next: pieces[first].off, end: pieces[last].off + pieces[last].n}
		a.advance(a.next)
		return a
	}
	i := first
	for i <= last {
		reach := exactReach(pieces, i, bufLen)
		adviseWillNeed(f, pieces[i].off, reach-pieces[i].off)
		i++
		for i <= last && (pieces[i].emit != nil || pieces[i].off+pieces[i].n <= reach) {
			i++ // within that read
		}
	}
	return nil
}

// networkSource is onNetwork, swappable for a test.
var networkSource = onNetwork

// remoteReads is how many ranges of one segment a remote source is asked for at once.
var remoteReads = 8

// remoteMaxRanges is the most requests an exact rendition's runs may take from a remote source: two round trips of
// remoteReads. Past it (audio stored sample by sample between video frames, 147 ranges a segment) one request reads
// through: measured at 10 ms a request, 200 ms a segment by ranges against 28 ms read through.
var remoteMaxRanges = 16

// rangeCount is how many reads the pieces take, holes up to gap read through.
func rangeCount(pieces []span, gap int64) int {
	n := 0
	for i := 0; i < len(pieces); {
		if pieces[i].emit != nil {
			i++
			continue
		}
		end := reachThrough(pieces, i, gap, parallelMaxBytes)
		n++
		for i++; i < len(pieces) && (pieces[i].emit != nil || pieces[i].off+pieces[i].n <= end); i++ {
		}
	}
	return n
}

// remoteReadGap is the largest hole between two runs of an exact rendition a remote source is asked for in one range.
var remoteReadGap int64 = 64 << 10

// Reads in flight at once for one segment on a network mount, and the most
// bytes they hold at once.
const (
	parallelReads    = 8
	parallelMaxBytes = 4 << 20
)

// copyParallel copies an exact rendition's pieces from ra, each read gathering
// the runs whose holes are at most gap, up to maxRead bytes: inFlight reads at
// once, parallelMaxBytes held at most, written in order.
func (c *spanCopier) copyParallel(ctx context.Context, w io.Writer, pieces []span, ra io.ReaderAt, gap, maxRead int64, inFlight int) error {
	type read struct{ off, end, at int64 }
	var reads []read
	for i := 0; i < len(pieces); {
		if pieces[i].emit != nil {
			i++
			continue
		}
		end := reachThrough(pieces, i, gap, maxRead)
		reads = append(reads, read{off: pieces[i].off, end: end})
		i++
		for i < len(pieces) && (pieces[i].emit != nil || pieces[i].off+pieces[i].n <= end) {
			i++ // within that read
		}
	}
	var buf []byte
	defer func() {
		if c.wipe {
			clear(buf)
		}
	}()
	// fetch reads reads[k:] in flight together, as many as parallelMaxBytes holds (one at least), and returns past the last.
	fetch := func(k int) (int, error) {
		var total int64
		last := k
		for last < len(reads) && (last == k || total+reads[last].end-reads[last].off <= parallelMaxBytes) {
			reads[last].at = total
			total += reads[last].end - reads[last].off
			last++
		}
		if int64(cap(buf)) < total {
			if c.wipe {
				clear(buf)
			}
			buf = make([]byte, total)
		}
		buf = buf[:total]
		errs := make([]error, last-k)
		sem := make(chan struct{}, inFlight)
		var wg sync.WaitGroup
		for x := k; x < last; x++ {
			if err := ctx.Err(); err != nil {
				wg.Wait()
				return last, err
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(x int) {
				defer func() { <-sem; wg.Done() }()
				r := reads[x]
				n, err := ra.ReadAt(buf[r.at:r.at+r.end-r.off], r.off)
				c.count(int64(n))
				if err != nil && (err != io.EOF || int64(n) < r.end-r.off) {
					errs[x-k] = err
				}
			}(x)
		}
		wg.Wait()
		return last, errors.Join(errs...)
	}
	k, fetched := 0, 0 // reads[k] holds the next piece; reads before fetched are in buf
	for _, pc := range pieces {
		if pc.emit != nil {
			if err := c.emit(w, pc.emit); err != nil {
				return err
			}
			continue
		}
		for pc.off >= reads[k].end {
			k++
		}
		if k >= fetched {
			if err := c.flush(w); err != nil { // the queued writes are slices of buf
				return err
			}
			var err error
			if fetched, err = fetch(k); err != nil {
				return err
			}
		}
		at := reads[k].at + pc.off - reads[k].off
		if err := c.queue(w, buf[at:at+pc.n]); err != nil {
			return err
		}
	}
	return c.flush(w) // before buf is wiped
}

// reachThrough is how far one read for run i may go: through the runs after it while the hole before each is at most gap and the read stays within maxRead.
func reachThrough(pieces []span, i int, gap, maxRead int64) int64 {
	start, end := pieces[i].off, pieces[i].off+pieces[i].n
	for _, pc := range pieces[i+1:] {
		if pc.emit != nil {
			continue
		}
		if pc.off < end || pc.off-end > gap || pc.off+pc.n-start > maxRead {
			break
		}
		end = pc.off + pc.n
	}
	return end
}

// exactReach is how far one read for run i may go: through the runs after it
// while the hole before each is at most exactReadGap and the read fits the
// buffer.
func exactReach(pieces []span, i, bufLen int) int64 {
	return reachThrough(pieces, i, exactReadGap, int64(bufLen))
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
	read     *atomic.Int64 // the plan's SourceReadBytes, when the copier serves a plan
}

// count adds n source bytes read to the plan's SourceReadBytes.
func (c *spanCopier) count(n int64) {
	if c.read != nil {
		c.read.Add(n)
	}
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
				c.count(m)
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
		c.count(int64(m))
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
		poison.Fill((*c.stage)[:cap(*c.stage)])
		stageBufs.Put(c.stage)
		c.stage = nil
	}
}

// putStreamBuf returns a copier buffer to its pool, zeroed when it held an encrypted segment's plaintext.
func putStreamBuf(bufp *[]byte, wipe bool) {
	if wipe {
		clear(*bufp)
	}
	poison.Fill(*bufp)
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
