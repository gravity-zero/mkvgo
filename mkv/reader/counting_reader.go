package reader

import (
	"errors"
	"io"
)

// countingReader is a forward-only buffered reader with an adaptive window.
// It tracks the logical position by counting consumed bytes; the invariant is
// src's file offset == pos + (w - r), i.e. the source sits at the end of the
// window. Skips drop window bytes for free and Seek past anything beyond  -
// bytes outside kept blocks are never read. The read-ahead starts small after
// a jump and doubles on every sequential fill (up to bufSize), so the reader
// behaves like a bulk sequential reader on dense data and like a sparse
// header-hopper on skippable data, whichever the stream turns out to be.
type countingReader struct {
	src   io.ReadSeeker
	buf   []byte // window storage, bufSize capacity
	r, w  int    // valid window is buf[r:w]; buf[r] is the byte at pos
	pos   int64  // logical position of the next byte the caller consumes
	end   int64  // source size, resolved on the first seek-skip (-1 until then) and kept: a reader never outlives a growth of its source, the growing paths building one per Cluster confirmed whole
	chunk int    // current read-ahead size; grows sequentially, shrinks on jumps
	// sparse seeks over every skipped byte past the window and keeps reads at sparseChunk (SetSparse).
	sparse bool
	nread  int64 // bytes read from src so far
	// seekable is false for a stream source (readOnlySeeker), whose skips are all read through.
	seekable bool
	failed   error // the first error src itself returned, other than its end: an I/O failure is never damage in the file
}

// fail remembers src's own failure (not its end).
func (c *countingReader) fail(err error) {
	if err != nil && c.failed == nil && !errors.Is(err, io.EOF) {
		c.failed = err
	}
}

func newCountingReader(src io.ReadSeeker, startPos int64) *countingReader {
	_, stream := src.(*readOnlySeeker)
	return &countingReader{
		src:      src,
		buf:      make([]byte, bufSize),
		pos:      startPos,
		end:      -1,
		chunk:    minChunk,
		seekable: !stream,
	}
}

// growChunk doubles the read-ahead after a sequential fill, up to bufSize.
func (c *countingReader) growChunk() {
	if c.sparse {
		c.chunk = sparseChunk
		return
	}
	if c.chunk < len(c.buf) {
		c.chunk *= 2
		if c.chunk > len(c.buf) {
			c.chunk = len(c.buf)
		}
	}
}

// fill loads the next chunk into the (empty) window.
func (c *countingReader) fill() error {
	n, err := c.src.Read(c.buf[:c.chunk])
	c.nread += int64(n)
	c.fail(err)
	if n == 0 {
		if err == nil {
			err = io.ErrNoProgress
		}
		return err
	}
	c.growChunk()
	c.r, c.w = 0, n
	return nil
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.r == c.w {
		if len(p) >= c.chunk {
			// Large read with an empty window: straight into p.
			n, err := c.src.Read(p)
			c.pos += int64(n)
			c.nread += int64(n)
			c.fail(err)
			c.growChunk()
			return n, err
		}
		if err := c.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, c.buf[c.r:c.w])
	c.r += n
	c.pos += int64(n)
	return n, nil
}

// ReadByte returns the next byte. It makes the reader an io.ByteReader, which
// is what lets the EBML number readers take a byte at a time from the window
// instead of passing a slice through io.Reader - an allocation per number.
func (c *countingReader) ReadByte() (byte, error) {
	if c.r == c.w {
		if err := c.fill(); err != nil {
			return 0, err
		}
	}
	b := c.buf[c.r]
	c.r++
	c.pos++
	return b, nil
}

// readBE reads n bytes (8 at most) as a big-endian value, with io.ReadFull's
// errors: io.EOF before the first byte, io.ErrUnexpectedEOF after it.
func (c *countingReader) readBE(n int) (uint64, error) {
	var val uint64
	for i := 0; i < n; i++ {
		b, err := c.ReadByte()
		if err != nil {
			if err == io.EOF && i > 0 {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		val = val<<8 | uint64(b)
	}
	return val, nil
}

// discard advances the reader by exactly n bytes without delivering them.
// The window's bytes are dropped for free. A remainder past the window is
// seeked over - never read - when it is large enough to beat the fixed
// round-trip cost of the next read (seekSkipMin); smaller remainders are read
// forward through the growing window, keeping the walk a bulk sequential
// read. A skip beyond the source's end errors like a truncated read would.
// A seek resets the window growth: the walk has proven sparse, keep the next
// reads small.
func (c *countingReader) discard(n int64) error {
	if n <= 0 {
		return nil
	}
	avail := int64(c.w - c.r)
	if n <= avail {
		c.r += int(n)
		c.pos += n
		return nil
	}
	if (n-avail <= seekSkipMin && !c.sparse) || !c.seekable {
		_, err := io.CopyN(io.Discard, c, n)
		if err == io.EOF {
			return io.ErrUnexpectedEOF // the source ends inside the bytes skipped, as on the seeking path
		}
		return err
	}
	n -= avail
	c.pos += avail
	c.r, c.w = 0, 0
	// The window is empty, so src's offset == c.pos: seek forward from there.
	if c.end < 0 {
		end, err := c.src.Seek(0, io.SeekEnd)
		if err != nil {
			c.fail(err)
			return err
		}
		c.end = end
	}
	target := c.pos + n
	if target > c.end {
		// The size probe above moved src to EOF; put it back where pos says it
		// is, or the invariant (src offset == pos + (w-r)) stays broken for
		// every later read on this reader.
		if _, serr := c.src.Seek(c.pos, io.SeekStart); serr != nil {
			c.fail(serr)
			return serr
		}
		return io.ErrUnexpectedEOF
	}
	if _, err := c.src.Seek(target, io.SeekStart); err != nil {
		c.fail(err)
		return err
	}
	c.pos = target
	c.chunk = c.restartChunk()
	return nil
}

// restartChunk is the read-ahead after a jump.
func (c *countingReader) restartChunk() int {
	if c.sparse {
		return sparseChunk
	}
	return minChunk
}

// reset re-seats the reader at pos, keeping the window storage. A walk that
// hops between recorded positions would otherwise allocate a fresh 256 KiB
// window per hop.
func (c *countingReader) reset(pos int64) error {
	if _, err := c.src.Seek(pos, io.SeekStart); err != nil {
		c.fail(err)
		return err
	}
	c.r, c.w = 0, 0
	c.pos = pos
	c.chunk = c.restartChunk()
	return nil
}

// tell returns the current byte position (offset from file start).
func (c *countingReader) tell() int64 {
	return c.pos
}
