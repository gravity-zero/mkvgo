package reader

import (
	"fmt"
	"io"
)

// readExactAllocEager is the upfront allocation ceiling for readExact: reads up
// to this size allocate in one shot (the common, fast case); above it the
// buffer grows only as bytes actually arrive.
const readExactAllocEager = 8 << 20

// readExact reads exactly n bytes from r into a fresh buffer. For a large n it
// grows the buffer as bytes arrive rather than allocating n up front, so a size
// declared larger than the source actually delivers (notably a hostile remote
// Content-Length, where the "declared size fits the file" guard is only as
// trustworthy as the server's reported length) fails at I/O time having
// allocated proportionally to the bytes received. A short read returns
// io.ErrUnexpectedEOF, matching io.ReadFull.
func readExact(r io.Reader, n int64) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("readExact: negative size %d", n)
	}
	if n <= readExactAllocEager {
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	// From 1 MiB, doubled as the bytes come: a size only declared (60 MiB for ten bytes) allocates the first step.
	buf := make([]byte, 0, readExactGrowFrom)
	for int64(len(buf)) < n {
		if len(buf) == cap(buf) {
			next := make([]byte, len(buf), min(n, 2*int64(cap(buf))))
			copy(next, buf)
			buf = next
		}
		got, err := io.ReadFull(r, buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+got]
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}
	return buf, nil
}

// readExactGrowFrom is the first step of a read past readExactAllocEager.
const readExactGrowFrom = 1 << 20
