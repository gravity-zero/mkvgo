package ebml

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// MaxElementSize is the maximum allowed allocation for a single element.
// Prevents OOM from malicious or corrupted files.
const MaxElementSize = 512 * 1024 * 1024 // 512 MB

func checkSize(size int64) error {
	if size < 0 || size > MaxElementSize {
		return fmt.Errorf("element size %d exceeds limit (%d bytes)", size, MaxElementSize)
	}
	return nil
}

// ReadUint reads an unsigned integer of the given byte size.
func ReadUint(r io.Reader, size int64) (uint64, error) {
	if size < 0 || size > 8 {
		return 0, fmt.Errorf("invalid uint size %d", size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, err
	}
	var val uint64
	for _, b := range buf {
		val = (val << 8) | uint64(b)
	}
	return val, nil
}

// ReadFloat reads a 4- or 8-byte IEEE 754 float.
func ReadFloat(r io.Reader, size int64) (float64, error) {
	if size == 4 {
		buf := make([]byte, 4)
		if _, err := io.ReadFull(r, buf); err != nil {
			return 0, err
		}
		return float64(math.Float32frombits(binary.BigEndian.Uint32(buf))), nil
	}
	if size != 8 {
		return 0, fmt.Errorf("invalid float size %d", size)
	}
	buf := make([]byte, 8)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.BigEndian.Uint64(buf)), nil
}

// ReadString reads a UTF-8/ASCII string, trimming trailing nulls.
func ReadString(r io.Reader, size int64) (string, error) {
	if err := checkSize(size); err != nil {
		return "", err
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	for len(buf) > 0 && buf[len(buf)-1] == 0 {
		buf = buf[:len(buf)-1]
	}
	return string(buf), nil
}

// ReadBytes reads raw bytes.
func ReadBytes(r io.Reader, size int64) ([]byte, error) {
	if err := checkSize(size); err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
