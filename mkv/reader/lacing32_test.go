package reader

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

// vint8 is v as an 8-byte EBML VINT.
func vint8(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], 1<<56|v)
	return b[:]
}

// TestLaceSizesDoNotWrapOn32Bit: lace sizes past 2^31 were taken as int before they were checked against the payload;
// on a 32-bit platform they wrapped to small or negative sizes that passed the checks. Each must be refused.
func TestLaceSizesDoNotWrapOn32Bit(t *testing.T) {
	bias := uint64(1)<<55 - 1
	xiph := append(bytes.Repeat([]byte{0xFF}, math.MaxInt32/255+2), 0x00)
	cases := []struct {
		name   string
		lacing byte
		raw    []byte
		frames int
	}{
		{"ebml first size 2^32+5", lacingEBML, append(vint8(1<<32+5), make([]byte, 64)...), 2},
		{"ebml diff +2^32", lacingEBML, append(append([]byte{0x8A}, vint8(bias+1<<32)...), make([]byte, 64)...), 3},
		{"xiph size past 2^31", lacingXiph, append(xiph, make([]byte, 64)...), 2},
	}
	for _, c := range cases {
		sizes, _, err := decodeLacingSizes(c.lacing, c.raw, c.frames)
		if err == nil {
			t.Errorf("%s: sizes %v accepted from a %d-byte payload", c.name, sizes, len(c.raw))
		}
	}
}

// TestLaceHeaderPastTheAddressableSize: a fixed lace of 2^33 bytes is sized 2^32 a frame where int holds it, and
// refused where it does not (it wrapped to frames of 0 bytes).
func TestLaceHeaderPastTheAddressableSize(t *testing.T) {
	sizes, _, err := readLaceHeader(bufio.NewReader(bytes.NewReader(nil)), lacingFixed, 2, 1<<33)
	if math.MaxInt < 1<<33 {
		if err == nil {
			t.Errorf("a lace of 2^33 bytes on a 32-bit int was sized %v", sizes)
		}
		return
	}
	if err != nil || len(sizes) != 2 || int64(sizes[0]) != 1<<32 {
		t.Errorf("sizes %v, %v; want two frames of 2^32 bytes", sizes, err)
	}
}
