package writer

import (
	"bytes"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// A Void pads a reserved slot: Finalize writes it back into the SeekHead
// reserve, EditInPlace writes it between the metadata and the clusters. It must
// therefore consume EXACTLY the byte budget it is given - one byte too many
// overwrites the head of the next element, one too few shifts every offset that
// follows. Sizes just past a VINT width boundary (129, 16386, ...) used to
// overrun by one: they are unreachable with a minimal-width size VINT.
func TestWriteVoidIsExactAndReadsBack(t *testing.T) {
	sizes := []int{2, 3, 4, 127, 128, 129, 130, 131, 254, 255, 256, 257,
		16381, 16382, 16383, 16384, 16385, 16386, 16387, 16388}
	for n := 2; n <= 2100; n++ {
		sizes = append(sizes, n)
	}

	for _, n := range sizes {
		var buf bytes.Buffer
		if err := WriteVoid(&buf, n); err != nil {
			t.Fatalf("WriteVoid(%d): %v", n, err)
		}
		if buf.Len() != n {
			t.Errorf("WriteVoid(%d) wrote %d bytes (%+d): it runs over its slot",
				n, buf.Len(), buf.Len()-n)
			continue
		}

		// The (possibly widened) size VINT must read back as a Void spanning
		// exactly those n bytes.
		r := bytes.NewReader(buf.Bytes())
		h, hdrLen, err := ebml.ReadElementHeader(r)
		if err != nil {
			t.Errorf("WriteVoid(%d) does not parse: %v", n, err)
			continue
		}
		if h.ID != mkv.IDVoid {
			t.Errorf("WriteVoid(%d) wrote element 0x%X, want Void", n, h.ID)
		}
		if got := int64(hdrLen) + h.Size; got != int64(n) {
			t.Errorf("WriteVoid(%d) parses as a %d-byte element", n, got)
		}
	}
}

// A Void of fewer than 2 bytes cannot exist (1 byte of ID + 1 of size is the
// floor), so WriteVoid writes nothing at all. Callers must not pad a 1-byte gap
// with it and assume the byte was filled.
func TestWriteVoidBelowTwoWritesNothing(t *testing.T) {
	for _, n := range []int{-1, 0, 1} {
		var buf bytes.Buffer
		if err := WriteVoid(&buf, n); err != nil {
			t.Fatalf("WriteVoid(%d): %v", n, err)
		}
		if buf.Len() != 0 {
			t.Errorf("WriteVoid(%d) wrote %d bytes, want none", n, buf.Len())
		}
	}
}
