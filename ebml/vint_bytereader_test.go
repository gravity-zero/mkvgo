package ebml

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// plainReader hides every method but Read, so the EBML readers take their
// io.Reader path.
type plainReader struct{ r io.Reader }

func (p plainReader) Read(b []byte) (int, error) { return p.r.Read(b) }

// A source that can hand out single bytes (the block walk's buffered reader,
// a bytes.Reader, a bufio.Reader) is read byte by byte - no buffer per number
// escaping to the heap - and must give, for every input, the very value,
// width and error the io.Reader path gives: truncations included.
func TestReadVINTByteReaderMatchesReader(t *testing.T) {
	inputs := [][]byte{
		{}, {0x00}, {0x81}, {0xFF}, {0x40}, {0x40, 0x01}, {0x7F, 0xFF}, {0x20}, {0x20, 0x00}, {0x20, 0x00, 0x01},
		{0x1A, 0x45, 0xDF, 0xA3}, {0x1A, 0x45, 0xDF}, {0x10, 0, 0, 1}, {0x08, 0, 0, 0, 1}, {0x08, 0, 0},
		{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, {0x01, 0xFF, 0xFF, 0xFF}, {0x01},
		{0x02, 1, 2, 3, 4, 5, 6}, {0x04, 1, 2, 3, 4, 5}, {0x81, 0x82, 0x83},
	}
	for _, in := range inputs {
		v1, n1, e1 := ReadVINT(plainReader{bytes.NewReader(in)})
		br := bytes.NewReader(in)
		v2, n2, e2 := ReadVINT(br)
		if v1 != v2 || n1 != n2 || !sameErr(e1, e2) {
			t.Errorf("ReadVINT(% x): reader = %d, %d, %v ; byte reader = %d, %d, %v", in, v1, n1, e1, v2, n2, e2)
		}
		if e2 == nil && br.Len() != len(in)-n2 {
			t.Errorf("ReadVINT(% x) consumed %d bytes, reports %d", in, len(in)-br.Len(), n2)
		}
		for size := int64(0); size <= 9; size++ {
			u1, e1 := ReadUint(plainReader{bytes.NewReader(in)}, size)
			u2, e2 := ReadUint(bytes.NewReader(in), size)
			if u1 != u2 || !sameErr(e1, e2) {
				t.Errorf("ReadUint(% x, %d): reader = %d, %v ; byte reader = %d, %v", in, size, u1, e1, u2, e2)
			}
		}
	}
}

func sameErr(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	for _, target := range []error{io.EOF, io.ErrUnexpectedEOF} {
		if errors.Is(a, target) != errors.Is(b, target) {
			return false
		}
	}
	return a.Error() == b.Error()
}

// Reading a number from a byte-capable source allocates nothing.
func TestReadVINTByteReaderDoesNotAllocate(t *testing.T) {
	data := bytes.Repeat([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x81, 0x40, 0x7F, 0x08, 1, 2, 3, 4}, 64)
	r := bytes.NewReader(data)
	allocs := testing.AllocsPerRun(50, func() {
		r.Reset(data)
		for {
			if _, _, err := ReadVINT(r); err != nil {
				break
			}
		}
	})
	if allocs > 0 {
		t.Errorf("%.0f allocations to read the numbers of a buffer, want none", allocs)
	}
}
