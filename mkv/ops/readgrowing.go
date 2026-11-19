package ops

import (
	"io"

	"github.com/gravity-zero/mkvgo/internal/poison"
)

// readGrowStep is how much a body read in place of a declared size grows its buffer at a time: a size the file only
// declares (forged, cut short) allocates in proportion to the bytes that arrive, not to the declaration.
const readGrowStep = 8 << 20

// readBody reads an element body of n declared bytes into buf, reused when it already holds n, grown as the bytes
// arrive otherwise. A short read returns what arrived with io.ErrUnexpectedEOF (or io.EOF when nothing did).
func readBody(r io.Reader, buf []byte, n int64) ([]byte, error) {
	if int64(cap(buf)) >= n {
		buf = poison.Reuse(buf, int(n))
		_, err := io.ReadFull(r, buf)
		return buf, err
	}
	buf = buf[:0]
	for int64(len(buf)) < n {
		step := min(n-int64(len(buf)), max(int64(len(buf)), readGrowStep))
		next := make([]byte, len(buf), len(buf)+int(step))
		copy(next, buf)
		got, err := io.ReadFull(r, next[len(next):cap(next)])
		buf = next[:len(next)+got]
		if err != nil {
			if err == io.EOF && len(buf) > 0 {
				err = io.ErrUnexpectedEOF
			}
			return buf, err
		}
	}
	return buf, nil
}
