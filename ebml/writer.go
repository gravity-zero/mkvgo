package ebml

import (
	"encoding/binary"
	"io"
	"math"
)

// ElementIDLen returns the encoded byte length of an element ID.
// EBML element IDs include the VINT marker bit, which determines width.
func ElementIDLen(id uint32) int {
	switch {
	case id >= 0x10000000:
		return 4
	case id >= 0x200000:
		return 3
	case id >= 0x4000:
		return 2
	default:
		return 1
	}
}

// DataSizeLen returns the minimal VINT width to encode size.
func DataSizeLen(size int64) int {
	for w := 1; w <= 8; w++ {
		if uint64(size) <= (uint64(1)<<uint(w*7))-2 {
			return w
		}
	}
	return 8
}

// UintLen returns the minimal byte count to encode val.
func UintLen(val uint64) int {
	if val == 0 {
		return 1
	}
	n := 0
	for v := val; v > 0; v >>= 8 {
		n++
	}
	return n
}

// ElementHeaderLen returns the total byte length of an encoded element header.
func ElementHeaderLen(id uint32, size int64) int {
	if size < 0 {
		return ElementIDLen(id) + 8
	}
	return ElementIDLen(id) + DataSizeLen(size)
}

// WriteElementID writes an EBML element ID.
func WriteElementID(w io.Writer, id uint32) (int, error) {
	n := ElementIDLen(id)
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], id)
	return w.Write(buf[4-n:])
}

// WriteDataSize writes an EBML data size as a VINT with minimal width.
// Pass -1 for unknown size (encoded as 8-byte all-ones).
func WriteDataSize(w io.Writer, size int64) (int, error) {
	if size < 0 {
		var buf = [8]byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
		return w.Write(buf[:])
	}
	width := DataSizeLen(size)
	val := uint64(size) | (uint64(1) << uint(width*7))
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], val)
	return w.Write(buf[8-width:])
}

// WriteElementHeader writes an element ID followed by its data size.
func WriteElementHeader(w io.Writer, id uint32, size int64) (int, error) {
	n1, err := WriteElementID(w, id)
	if err != nil {
		return n1, err
	}
	n2, err := WriteDataSize(w, size)
	return n1 + n2, err
}

// WriteUint writes an unsigned integer in big-endian using n bytes.
func WriteUint(w io.Writer, val uint64, n int) (int, error) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], val)
	return w.Write(buf[8-n:])
}

// WriteFloat writes a float64 as 8-byte IEEE 754 big-endian.
func WriteFloat(w io.Writer, val float64) (int, error) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], math.Float64bits(val))
	return w.Write(buf[:])
}

// WriteFloat32 writes a float32 as 4-byte IEEE 754 big-endian.
func WriteFloat32(w io.Writer, val float32) (int, error) {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], math.Float32bits(val))
	return w.Write(buf[:])
}

// WriteString writes a UTF-8 string as raw bytes.
func WriteString(w io.Writer, val string) (int, error) {
	return io.WriteString(w, val)
}

// WriteBytes writes raw bytes.
func WriteBytes(w io.Writer, val []byte) (int, error) {
	return w.Write(val)
}
