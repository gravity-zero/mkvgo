package mp4

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"io"
)

// hlsstream_crypt.go - AES-128 segments written from the source as Open's
// handles serve them, byte for byte what Resource builds in memory.

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
	cw := &cbcWriter{w: w, mode: cipher.NewCBCEncrypter(block, segmentIV(k, uint32(h.n))), off: off, end: off + n, scratch: make([]byte, stageBytes)}
	defer clear(cw.scratch)
	plain := int64(len(h.head)) + h.table.segs[h.ti].dataLen
	need := min(plain, (off+n+aes.BlockSize-1)/aes.BlockSize*aes.BlockSize) // the plaintext the range's last block chains from
	if _, err := p.writeSpans(ctx, cw, h.head, h.table, h.ti, 0, need); err != nil && !errors.Is(err, errRangeWritten) {
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
	m, err := c.w.Write(s[from-(c.pos-int64(len(s))) : to-(c.pos-int64(len(s)))])
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
