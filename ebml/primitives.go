package ebml

import (
	"bytes"
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
	if br, ok := r.(io.ByteReader); ok {
		return readBE(br, int(size)) // no buffer to allocate: see ReadVINT
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

// ReadFloat reads a 4- or 8-byte IEEE 754 float; a 0-byte float is 0 (RFC 8794 section 7.3).
func ReadFloat(r io.Reader, size int64) (float64, error) {
	if size == 0 {
		return 0, nil
	}
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

// readExact reads exactly size bytes. For sizes above a small threshold it
// grows the buffer as data arrives instead of allocating size bytes upfront,
// so a malformed element that declares a huge size but supplies little data
// cannot force a giant allocation (memory DoS). The growth is its own, not the
// stdlib's: doubling while full, then size at once when an eighth of it has
// arrived, so under 1.5 times size is allocated in all and never more than
// 8 times what arrived.
func readExact(r io.Reader, size int64) ([]byte, error) {
	if err := checkSize(size); err != nil {
		return nil, err
	}
	const maxUpfront = 1 << 20 // 1 MiB: allocate exactly for the common small case
	if size <= maxUpfront {
		buf := make([]byte, size)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	buf := make([]byte, 0, maxUpfront)
	for int64(len(buf)) < size {
		if len(buf) == cap(buf) {
			next := 2 * int64(cap(buf))
			if 4*next >= size {
				next = size
			}
			grown := make([]byte, len(buf), next)
			copy(grown, buf)
			buf = grown
		}
		n, err := io.ReadFull(r, buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// ReadString reads a UTF-8/ASCII string up to its first null, past which nothing belongs to it (RFC 8794 section 13).
func ReadString(r io.Reader, size int64) (string, error) {
	buf, err := readExact(r, size)
	if err != nil {
		return "", err
	}
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf), nil
}

// ReadBytes reads raw bytes.
func ReadBytes(r io.Reader, size int64) ([]byte, error) {
	return readExact(r, size)
}
