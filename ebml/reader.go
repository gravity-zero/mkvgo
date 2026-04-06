package ebml

import (
	"fmt"
	"io"
)

// ReadVINT reads a variable-length integer. Returns value and bytes consumed.
func ReadVINT(r io.Reader) (uint64, int, error) {
	var first [1]byte
	if _, err := io.ReadFull(r, first[:]); err != nil {
		return 0, 0, err
	}
	b := first[0]
	if b == 0 {
		return 0, 0, fmt.Errorf("invalid VINT: leading zero byte")
	}

	width := 1
	for i := 7; i >= 0; i-- {
		if b&(1<<uint(i)) != 0 {
			width = 8 - i
			break
		}
	}

	val := uint64(b)
	if width > 1 {
		rest := make([]byte, width-1)
		if _, err := io.ReadFull(r, rest); err != nil {
			return 0, 0, err
		}
		for _, rb := range rest {
			val = (val << 8) | uint64(rb)
		}
	}

	return val, width, nil
}

// ReadElementID reads an EBML element ID.
func ReadElementID(r io.Reader) (uint32, int, error) {
	val, n, err := ReadVINT(r)
	if err != nil {
		return 0, 0, err
	}
	return uint32(val), n, nil
}

// ReadDataSize reads an EBML data size, stripping the VINT marker bit.
// Returns -1 for unknown-size elements.
func ReadDataSize(r io.Reader) (int64, int, error) {
	val, n, err := ReadVINT(r)
	if err != nil {
		return 0, 0, err
	}
	mask := uint64(1) << uint(n*7)
	size := int64(val & ^mask)
	if val == mask|(mask-1) {
		return -1, n, nil
	}
	return size, n, nil
}

// ElementHeader holds a parsed EBML element ID and data size.
type ElementHeader struct {
	ID   uint32
	Size int64
}

// ReadElementHeader reads an element ID + data size from r.
func ReadElementHeader(r io.Reader) (ElementHeader, int, error) {
	id, n1, err := ReadElementID(r)
	if err != nil {
		return ElementHeader{}, 0, err
	}
	size, n2, err := ReadDataSize(r)
	if err != nil {
		return ElementHeader{}, 0, err
	}
	return ElementHeader{ID: id, Size: size}, n1 + n2, nil
}
