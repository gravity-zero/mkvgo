package mp4

import (
	"bytes"
	"io"
)

// readExactAllocEager is the upfront allocation ceiling for readExact: reads up
// to this size allocate their buffer in one shot (the common, fast case); above
// it, the buffer grows only as bytes actually arrive.
const readExactAllocEager = 8 << 20

// readExact reads exactly n bytes from r into a fresh buffer. For a large n it
// grows the buffer as bytes arrive rather than allocating n up front, so a size
// declared larger than the source actually delivers - notably a hostile remote
// Content-Length, where the on-disk "offset + box size <= file size" guard is
// only as trustworthy as the server's reported length - fails at I/O time
// having allocated proportionally to the bytes received, not to the declared
// size. A short read returns io.ErrUnexpectedEOF, matching io.ReadFull.
func readExact(r io.Reader, n int64) ([]byte, error) {
	if n < 0 {
		return nil, errf("readExact: negative size %d", n)
	}
	if n <= readExactAllocEager {
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	var buf bytes.Buffer
	buf.Grow(readExactAllocEager)
	if _, err := io.CopyN(&buf, r, n); err != nil {
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf.Bytes(), nil
}
