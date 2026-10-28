package ebml

import (
	"bytes"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/costguard"
)

// FuzzReadElementHeader: any bytes give an element header or an error, never a panic; a header read says how many
// bytes it took, no more than were there, and a known size is never negative.
func FuzzReadElementHeader(f *testing.F) {
	for _, seed := range [][]byte{
		{0x1A, 0x45, 0xDF, 0xA3, 0x84}, {0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		{0xEC, 0x80}, {0x00}, {0xFF}, {0x40, 0x00, 0x01, 0xFF}, {},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		costguard.Fuzz(t, len(data), func() {
			h, n, err := ReadElementHeader(bytes.NewReader(data))
			if err != nil {
				return
			}
			if n <= 0 || n > len(data) {
				t.Fatalf("header of %d bytes read from %d", n, len(data))
			}
			if h.Size < -1 {
				t.Fatalf("size %d", h.Size)
			}
		})
	})
}

// FuzzReadPrimitives: an element body of any bytes read as each primitive type gives a value or an error, never a
// panic, and never allocates past what the body holds.
func FuzzReadPrimitives(f *testing.F) {
	f.Add([]byte{0x01, 0x02}, int64(2))
	f.Add([]byte{0x3F, 0xF0, 0, 0, 0, 0, 0, 0}, int64(8))
	f.Add([]byte("text\x00junk"), int64(9))
	f.Add([]byte{}, int64(1<<40))
	f.Fuzz(func(t *testing.T, data []byte, size int64) {
		costguard.Fuzz(t, len(data), func() {
			_, _ = ReadUint(bytes.NewReader(data), size)
			_, _ = ReadFloat(bytes.NewReader(data), size)
			_, _ = ReadString(bytes.NewReader(data), size)
			if b, err := ReadBytes(bytes.NewReader(data), size); err == nil && int64(len(b)) > int64(len(data)) {
				t.Fatalf("%d bytes read from %d", len(b), len(data))
			}
		})
	})
}
