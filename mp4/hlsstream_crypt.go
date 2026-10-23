package mp4

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/gravity-zero/mkvgo/internal/poison"
)

// hlsstream_crypt.go - encrypted segments written from the source as Open's
// handles serve them, byte for byte what Resource builds in memory.

// sampleBufs hold one sample at a time for the CENC passes; cencWriters gather a CENC segment's writes.
var (
	sampleBufs  = sync.Pool{New: func() any { b := make([]byte, 0, 256<<10); return &b }}
	cencWriters = sync.Pool{New: func() any { return bufio.NewWriterSize(nil, stageBytes) }}
)

// putSampleBuf returns a sample buffer to its pool, zeroed: it held a sample's plaintext.
func putSampleBuf(bufp *[]byte) {
	clear((*bufp)[:cap(*bufp)])
	poison.Fill((*bufp)[:cap(*bufp)])
	*bufp = (*bufp)[:0]
	sampleBufs.Put(bufp)
}

// cencStream is a CENC segment's state between Open and its writes: the
// track's cipher and the senc entries the first pass derived.
type cencStream struct {
	k  *cencCipher
	td *cencTrafData
}

// openCENC reads h's samples once to derive their senc entries (a video
// sample's subsamples come from its own bytes) and frames the segment's head
// with them: the moof must hold them before the first media byte goes out.
func (p *HLSPlan) openCENC(ctx context.Context, h *ResourceHandle) error {
	pt := p.tracks[h.ti]
	k, err := newCENCCipher(p.opts.CENC, pt.ft.outTrack.spec.video, pt.ft.outTrack.mkv.Codec, pt.ft.outTrack.mkv.CodecPrivate, pt.ft.outTrack.mkv.ID)
	if err != nil {
		return err
	}
	seg := h.table.segs[h.ti]
	td := &cencTrafData{hasSubsample: k.isVideo, entries: make([]sencEntry, len(seg.samples))}
	var rd *sampleReader
	if k.isVideo {
		if rd, err = p.newSampleReader(ctx, h.table, h.ti); err != nil {
			return err
		}
		defer rd.close()
	}
	bufp := sampleBufs.Get().(*[]byte)
	defer putSampleBuf(bufp)
	buf := (*bufp)[:0]
	for x, s := range seg.samples {
		if k.isVideo {
			if err := ctx.Err(); err != nil {
				return err
			}
			if buf, err = rd.read(x, buf); err != nil {
				return err
			}
			*bufp = buf
		}
		if td.entries[x], err = k.sampleEntry(buf, s); err != nil {
			return err
		}
	}
	seg.cenc = td
	h.head = buildSegmentFile(uint32(h.n+1), seg)
	h.cenc = &cencStream{k: k, td: td}
	return nil
}

// writeCENC writes [off, off+n) of a CENC segment: the head, then each sample
// the range meets read, encrypted on its own and written; the samples before
// the range are neither read nor encrypted.
func (p *HLSPlan) writeCENC(ctx context.Context, w io.Writer, h *ResourceHandle, off, n int64) (int64, error) {
	cw := &countWriter{w: w}
	bw := cencWriters.Get().(*bufio.Writer)
	bw.Reset(cw)
	defer func() {
		bw.Reset(nil) // its buffer held ciphertext and clear headers only
		cencWriters.Put(bw)
	}()
	end := off + n
	write := func(b []byte, pos int64) error {
		from, to := max(off, pos), min(end, pos+int64(len(b)))
		if from >= to {
			return nil
		}
		_, err := bw.Write(b[from-pos : to-pos])
		return err
	}
	if err := write(h.head, 0); err != nil {
		return cw.pos, err
	}
	pos := int64(len(h.head))
	var rd *sampleReader
	seq := cryptSeq.Add(1)
	if err := cryptGate.acquire(ctx, seq); err != nil {
		return 0, err
	}
	held := true
	defer func() {
		if held {
			cryptGate.release()
		}
	}()
	bufp := sampleBufs.Get().(*[]byte)
	defer putSampleBuf(bufp)
	buf := (*bufp)[:0]
	for x, s := range h.table.segs[h.ti].samples {
		size := int64(s.size)
		if pos+size <= off {
			pos += size
			continue
		}
		if pos >= end {
			break
		}
		if err := ctx.Err(); err != nil {
			return cw.pos, err
		}
		if rd == nil {
			var err error
			if rd, err = p.newSampleReader(ctx, h.table, h.ti); err != nil {
				return cw.pos, err
			}
			defer rd.close()
		}
		var err error
		if buf, err = rd.read(x, buf); err != nil {
			return cw.pos, err
		}
		*bufp = buf
		h.cenc.k.encrypt(buf, h.cenc.td.entries[x])
		cryptGate.release()
		held = false
		if err := write(buf, pos); err != nil {
			return cw.pos, err
		}
		if err := cryptGate.acquire(ctx, seq); err != nil {
			return cw.pos, err
		}
		held = true
		pos += size
	}
	cryptGate.release() // the last bytes go to the client without the slot
	held = false
	err := bw.Flush()
	return cw.pos, err
}

// writeAES writes [off, off+n) of an AES-128 segment: the plaintext segment,
// head and samples from the source, goes through CBC as it is produced, from
// its first byte (each block chains on the one before), and only the
// ciphertext the range covers is written; the PKCS#7 block closes it.
func (p *HLSPlan) writeAES(ctx context.Context, w io.Writer, h *ResourceHandle, off, n int64) (int64, error) {
	k := p.opts.Encrypt.periodKey(h.n)
	block, err := aes.NewCipher(k.Key)
	if err != nil {
		return 0, err
	}
	cw := &cbcWriter{w: w, mode: cipher.NewCBCEncrypter(block, segmentIV(k, uint32(h.n))), off: off, end: off + n, scratch: make([]byte, stageBytes), ctx: ctx, seq: cryptSeq.Add(1)}
	defer clear(cw.scratch)
	if err := cryptGate.acquire(ctx, cw.seq); err != nil {
		return 0, err
	}
	cw.held = true
	defer func() {
		if cw.held {
			cryptGate.release()
		}
	}()
	plain := int64(len(h.head)) + h.table.segs[h.ti].dataLen
	need := min(plain, (off+n+aes.BlockSize-1)/aes.BlockSize*aes.BlockSize) // the plaintext the range's last block chains from
	if _, err := p.writeSpans(ctx, cw, h.head, h.table, h.ti, h.n, 0, need); err != nil && !errors.Is(err, errRangeWritten) {
		return cw.written, err
	}
	if cw.pos < cw.end {
		if err := cw.close(); err != nil && !errors.Is(err, errRangeWritten) {
			return cw.written, err
		}
	}
	return cw.written, nil
}

// errRangeWritten stops the plaintext once the ciphertext range is out.
var errRangeWritten = errors.New("mp4: range written")

// cbcWriter encrypts what it is given with CBC and writes the part of the
// ciphertext that falls in [off, end); close pads the last block (PKCS#7).
type cbcWriter struct {
	w        io.Writer
	mode     cipher.BlockMode
	off, end int64 // the ciphertext range to write
	pos      int64 // ciphertext produced so far
	carry    [aes.BlockSize]byte
	nc       int
	scratch  []byte
	written  int64
	ctx      context.Context
	seq      int64 // the request's place in cryptGate's order
	held     bool
}

func (c *cbcWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		if c.pos >= c.end {
			return total, errRangeWritten
		}
		s := append(c.scratch[:0], c.carry[:c.nc]...)
		take := min(len(p), cap(c.scratch)-len(s))
		s = append(s, p[:take]...)
		p = p[take:]
		full := len(s) / aes.BlockSize * aes.BlockSize
		c.nc = copy(c.carry[:], s[full:])
		if err := c.emit(s[:full]); err != nil {
			return total - len(p), err
		}
	}
	return total, nil
}

// emit encrypts whole blocks in place and writes their part of the range.
func (c *cbcWriter) emit(s []byte) error {
	if len(s) == 0 {
		return nil
	}
	c.mode.CryptBlocks(s, s)
	from, to := max(c.off, c.pos), min(c.end, c.pos+int64(len(s)))
	c.pos += int64(len(s))
	if from >= to {
		return nil
	}
	if c.held {
		cryptGate.release()
		c.held = false
	}
	m, err := c.w.Write(s[from-(c.pos-int64(len(s))) : to-(c.pos-int64(len(s)))])
	if aerr := cryptGate.acquire(c.ctx, c.seq); aerr == nil {
		c.held = true
	} else if err == nil {
		err = aerr
	}
	c.written += int64(m)
	if err == nil && int64(m) < to-from {
		err = io.ErrShortWrite
	}
	return err
}

// close pads the carried bytes to a whole block and writes it.
func (c *cbcWriter) close() error {
	pad := aes.BlockSize - c.nc
	s := append(c.scratch[:0], c.carry[:c.nc]...)
	for range pad {
		s = append(s, byte(pad))
	}
	c.nc = 0
	return c.emit(s)
}

// sampleReader reads one sample of a segment at a time, whole and as served:
// a stripped header first, then its bytes from the source, through a pooled
// copier buffer.
type sampleReader struct {
	p      *HLSPlan
	t      *windowTable
	ti     int
	c      *spanCopier
	src    io.Closer
	bufp   *[]byte
	prefix []byte
	pieces []span // one per sample, for the copier's read-ahead bound
	ahead  *ahead // a video rendition read ahead of the samples, nil otherwise
}

func (p *HLSPlan) newSampleReader(ctx context.Context, t *windowTable, ti int) (*sampleReader, error) {
	src, err := p.openSource(ctx)
	if err != nil {
		return nil, err
	}
	r := &sampleReader{p: p, t: t, ti: ti, src: src, bufp: streamBufs.Get().(*[]byte), prefix: p.tracks[ti].ft.outTrack.mkv.HeaderStripping}
	r.c = &spanCopier{src: src, buf: *r.bufp, exact: !p.tracks[ti].ft.outTrack.spec.video, wipe: true, read: &p.copyRead}
	if f, ok := src.(*os.File); ok {
		r.c.file = f
	}
	samples := t.segs[ti].samples
	r.c.end = spansEnd(src, t, ti, 0, 0, 1<<62, int64(len(r.prefix)))
	r.pieces = make([]span, len(samples))
	for x, s := range samples {
		r.pieces[x] = span{off: t.offs[ti][x], n: int64(s.size) - int64(len(r.prefix))}
	}
	if r.c.file != nil && !p.onNetwork(r.c.file) {
		r.ahead = prefetch(r.c.file, r.pieces, r.c.exact, len(r.c.buf))
	}
	return r, nil
}

// read returns sample x's bytes in dst's storage.
func (r *sampleReader) read(x int, dst []byte) ([]byte, error) {
	a := &appendWriter{b: append(dst[:0], r.prefix...)}
	if r.c.exact {
		r.c.reach = exactReach(r.pieces, x, len(r.c.buf))
	}
	r.ahead.advance(r.pieces[x].off)
	if err := r.c.copy(a, r.pieces[x].off, r.pieces[x].n); err != nil {
		return a.b, err
	}
	err := r.c.flush(a)
	return a.b, err
}

func (r *sampleReader) close() {
	r.c.release()
	putStreamBuf(r.bufp, true)
	r.src.Close()
}

// appendWriter appends what it is given to b.
type appendWriter struct{ b []byte }

func (a *appendWriter) Write(p []byte) (int, error) {
	a.b = append(a.b, p...)
	return len(p), nil
}
