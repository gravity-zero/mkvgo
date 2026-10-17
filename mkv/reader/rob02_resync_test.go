package reader

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// overReadGuard counts the bytes read through it and fails once they pass budget.
type overReadGuard struct {
	r      io.ReadSeeker
	read   int64
	budget int64
}

var errOverRead = errors.New("read past the budget")

func (c *overReadGuard) Read(p []byte) (int, error) {
	if c.read > c.budget {
		return 0, errOverRead
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read > c.budget {
		return n, errOverRead
	}
	return n, err
}

func (c *overReadGuard) Seek(off int64, whence int) (int64, error) { return c.r.Seek(off, whence) }

// fakeMagics is n bytes of Cluster IDs each followed by a zero byte, which no size decodes from.
func fakeMagics(n int) []byte {
	return bytes.Repeat([]byte{0x1F, 0x43, 0xB6, 0x75, 0x00}, n/5)
}

// timedCluster is a Cluster opening with its Timestamp.
var timedCluster = []byte{0x1F, 0x43, 0xB6, 0x75, 0x83, 0xE7, 0x81, 0x00}

func TestROB02_ResyncReadsOnce(t *testing.T) {
	region := fakeMagics(8 << 20)
	data := append(append([]byte{}, region...), timedCluster...)
	for _, limit := range []int64{-1, int64(len(data))} {
		cr := &overReadGuard{r: bytes.NewReader(data), budget: 2 * int64(len(region))}
		off, err := ResyncToCluster(cr, limit)
		if err != nil {
			t.Fatalf("limit %d: %v after %d bytes read", limit, err, cr.read)
		}
		if off != int64(len(region)) {
			t.Fatalf("limit %d: resync at %d, want %d", limit, off, len(region))
		}
		if pos, _ := cr.Seek(0, io.SeekCurrent); pos != off {
			t.Fatalf("left at %d, want %d", pos, off)
		}
	}
}

func TestROB02_SegmentResyncReadsOnce(t *testing.T) {
	region := fakeMagics(8 << 20)
	data := append(append([]byte{}, region...), timedCluster...)
	cr := &overReadGuard{r: bytes.NewReader(data), budget: 2 * int64(len(region))}
	off, err := ResyncToSegmentElement(cr, -1)
	if err != nil {
		t.Fatalf("%v after %d bytes read", err, cr.read)
	}
	if off != int64(len(region)) {
		t.Fatalf("resync at %d, want %d", off, len(region))
	}
}
