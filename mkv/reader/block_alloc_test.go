package reader

import (
	"bytes"
	"runtime"
	"testing"
)

// A block's declared size allocates no more than the bytes the file delivers: a few bytes declaring a 60 MiB block
// inside a cluster of unknown size allocated 60 MiB on every walk that met them.
func TestBlockReaderAllocatesNoDeclaredBlockSize(t *testing.T) {
	for name, flags := range map[string]byte{"unlaced": 0x80, "laced": 0x82} {
		t.Run(name, func(t *testing.T) {
			data := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x80, // EBML header, empty
				0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, // Segment, unknown size
				0x1F, 0x43, 0xB6, 0x75, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, // Cluster, unknown size
				0xE7, 0x81, 0x00, // Timestamp 0
				0xA3, 0x13, 0xC0, 0x00, 0x00, 0x81, 0x00, 0x00, flags, // SimpleBlock declaring 60 MiB, track 1
				0x00, 1, 2, 3, 4, 5, 6, 7, 8, 9} // ten bytes, then the file ends
			br, err := NewBlockReader(bytes.NewReader(data), 1_000_000)
			if err != nil {
				t.Fatal(err)
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			if _, err := br.Next(); err == nil {
				t.Fatal("a block cut short must not read")
			}
			runtime.ReadMemStats(&after)
			if got := after.TotalAlloc - before.TotalAlloc; got > 16<<20 {
				t.Errorf("%d bytes allocated for a %d-byte file", got, len(data))
			}
		})
	}
}
