package mp4

import "io"

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
	buf := make([]byte, 0, readExactAllocEager)
	for int64(len(buf)) < n {
		if len(buf) == cap(buf) {
			// Doubling, capped at n: the last growth allocates what is left to reach n, never twice the box.
			grown := make([]byte, len(buf), min(2*int64(cap(buf)), n))
			copy(grown, buf)
			buf = grown
		}
		m, err := r.Read(buf[len(buf):min(int64(cap(buf)), n)])
		buf = buf[:len(buf)+m]
		if err == io.EOF && int64(len(buf)) < n {
			return nil, io.ErrUnexpectedEOF
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
	}
	return buf, nil
}
