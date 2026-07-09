package mp4

import (
	"bytes"
	"io"
	"runtime"
	"testing"
)

func TestReadExact_ExactBytes(t *testing.T) {
	for _, n := range []int64{0, 1, 100, readExactAllocEager, readExactAllocEager + 1, readExactAllocEager * 2} {
		src := bytes.Repeat([]byte{0xAB}, int(n))
		got, err := readExact(bytes.NewReader(src), n)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if int64(len(got)) != n || !bytes.Equal(got, src) {
			t.Fatalf("n=%d: got %d bytes, mismatch", n, len(got))
		}
	}
}

func TestReadExact_ShortReadIsUnexpectedEOF(t *testing.T) {
	// Small path (single alloc) and large path (growing) both report a short
	// read as io.ErrUnexpectedEOF, like io.ReadFull.
	for _, n := range []int64{1000, readExactAllocEager * 3} {
		src := bytes.Repeat([]byte{1}, 500) // fewer than n
		if _, err := readExact(bytes.NewReader(src), n); err != io.ErrUnexpectedEOF {
			t.Fatalf("n=%d: want ErrUnexpectedEOF, got %v", n, err)
		}
	}
}

// TestReadExact_ShortDeliveryDoesNotOverAllocate is the regression guard for the
// remote-streaming DoS seam: a size declared far larger than the source delivers
// (a hostile Content-Length) must not allocate the declared size up front. The
// old make([]byte, declared) would allocate ~500 MiB here.
func TestReadExact_ShortDeliveryDoesNotOverAllocate(t *testing.T) {
	const declared = 500 << 20 // claims 500 MiB
	const delivered = 4 << 10  // source only has 4 KiB, then EOF
	src := bytes.NewReader(bytes.Repeat([]byte{7}, delivered))

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	_, err := readExact(src, declared)
	runtime.ReadMemStats(&m1)

	if err != io.ErrUnexpectedEOF {
		t.Fatalf("want ErrUnexpectedEOF, got %v", err)
	}
	alloc := m1.TotalAlloc - m0.TotalAlloc
	if alloc > 64<<20 { // must be far below the 500 MiB declared
		t.Errorf("allocated %d bytes for a %d-byte short read of a %d-declared size; the declared size must not be allocated up front", alloc, delivered, int64(declared))
	}
}
