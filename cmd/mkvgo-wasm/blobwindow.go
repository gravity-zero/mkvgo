package main

const (
	blobWindowMin = 64 << 10
	blobWindowMax = 512 << 10
	blobWindows   = 4 // windows kept at once, for reads that alternate between regions
)

// nextBlobWindow sizes the Blob read at off: it doubles while the reads run on from the previous one
// (prevEnd), up to blobWindowMax, and starts at blobWindowMin after a jump.
func nextBlobWindow(prevEnd, off int64, prev int) int {
	if prev > 0 && off == prevEnd {
		return min(2*prev, blobWindowMax)
	}
	return blobWindowMin
}

// blobWin is one cached window of a Blob: buf holds the bytes from off.
type blobWin struct {
	buf  []byte
	off  int64
	win  int
	used uint64
}

// blobCache keeps a few Blob windows, so reads that alternate between file regions each keep theirs.
type blobCache struct {
	wins [blobWindows]blobWin
	tick uint64
}

// read copies into p from pos, loading through fetch the window a miss needs: the one pos continues, else the least recently used.
func (c *blobCache) read(p []byte, pos, size int64, fetch func(dst []byte, off int64) ([]byte, error)) (int, error) {
	c.tick++
	var w *blobWin
	for i := range c.wins {
		if x := &c.wins[i]; pos >= x.off && pos < x.off+int64(len(x.buf)) {
			w = x
			break
		}
	}
	if w == nil {
		w = &c.wins[0]
		for i := range c.wins {
			x := &c.wins[i]
			if x.win > 0 && pos == x.off+int64(len(x.buf)) {
				w = x
				break
			}
			if x.used < w.used {
				w = x
			}
		}
		w.win = nextBlobWindow(w.off+int64(len(w.buf)), pos, w.win)
		n := int(min(int64(w.win), size-pos))
		if cap(w.buf) < n {
			w.buf = make([]byte, n)
		}
		buf, err := fetch(w.buf[:n], pos)
		if err != nil {
			w.buf, w.win = w.buf[:0], 0
			return 0, err
		}
		w.buf, w.off = buf, pos
	}
	w.used = c.tick
	return copy(p, w.buf[pos-w.off:]), nil
}

// holds reports whether one window already read covers n bytes at off.
func (c *blobCache) holds(off, n int64) bool {
	for _, w := range c.wins {
		if off >= w.off && off+n <= w.off+int64(len(w.buf)) {
			return true
		}
	}
	return false
}
