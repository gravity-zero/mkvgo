package ebml

import (
	"fmt"
	"io"
)

// readBE reads n bytes (8 at most) from br as a big-endian value, with
// io.ReadFull's convention: io.EOF when the source ends before the first
// byte, io.ErrUnexpectedEOF when it ends after it.
func readBE(br io.ByteReader, n int) (uint64, error) {
	var val uint64
	for i := 0; i < n; i++ {
		b, err := br.ReadByte()
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

// ReadVINT reads a variable-length integer. Returns value and bytes consumed.
//
// A source that hands out single bytes (io.ByteReader) is read that way: a
// slice passed to an io.Reader escapes to the heap, which made every number
// read an allocation - some ten thousand per media segment walked.
func ReadVINT(r io.Reader) (uint64, int, error) {
	if br, ok := r.(io.ByteReader); ok {
		b, err := br.ReadByte()
		if err != nil {
			return 0, 0, err
		}
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
		rest, err := readBE(br, width-1)
		if err != nil {
			return 0, 0, err
		}
		return uint64(b)<<(8*uint(width-1)) | rest, width, nil
	}
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
		var rest [7]byte // width is 1..8, so width-1 fits; avoids a heap alloc per VINT
		if _, err := io.ReadFull(r, rest[:width-1]); err != nil {
			return 0, 0, err
		}
		for _, rb := range rest[:width-1] {
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
	if n > 4 {
		// EBML element IDs are 1-4 octets; a wider VINT would silently truncate
		// into uint32. Reject it rather than corrupt the parse.
		return 0, n, fmt.Errorf("invalid element ID: %d-octet VINT exceeds 4-octet limit", n)
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
