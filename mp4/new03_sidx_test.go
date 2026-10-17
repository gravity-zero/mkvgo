package mp4

import (
	"encoding/binary"
	"io"
	"testing"
)

// zeroTail serves head, then zeros up to size: a box that declares its bytes and delivers them.
type zeroTail struct {
	head []byte
	size int64
	pos  int64
}

func (z *zeroTail) Read(p []byte) (int, error) {
	if z.pos >= z.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), z.size-z.pos))
	for i := range p[:n] {
		p[i] = 0
		if at := z.pos + int64(i); at < int64(len(z.head)) {
			p[i] = z.head[at]
		}
	}
	z.pos += int64(n)
	return n, nil
}

func (z *zeroTail) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		z.pos = off
	case io.SeekCurrent:
		z.pos += off
	case io.SeekEnd:
		z.pos = z.size + off
	}
	return z.pos, nil
}

func TestNEW03_SidxSizeBound(t *testing.T) {
	const sidxBytes = 64 << 20
	head := make([]byte, 24)
	binary.BigEndian.PutUint32(head[0:], 16)
	copy(head[4:], "ftypisom")
	binary.BigEndian.PutUint32(head[16:], sidxBytes)
	copy(head[20:], "sidx")
	src := &zeroTail{head: head, size: 16 + sidxBytes}
	before := totalAlloc()
	got := scanSidxBoxes(src, src.size)
	if grew := totalAlloc() - before; grew > 8<<20 {
		t.Fatalf("a %d MiB box named sidx allocated %d MiB; a sidx holds at most %d bytes", sidxBytes>>20, grew>>20, maxSidxBytes)
	}
	if len(got) != 0 {
		t.Fatal("a sidx past the size its 16-bit reference count allows was returned")
	}
}
