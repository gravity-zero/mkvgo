package reader

import (
	"testing"
)

// voidTail is n bytes of empty Voids (EC 80) closed by a stray byte, so no run tiles to the end.
func voidTail(n int) []byte {
	b := make([]byte, n)
	for i := 0; i+1 < n; i += 2 {
		b[i], b[i+1] = 0xEC, 0x80
	}
	b[n-1] = 0x00
	return b
}

func BenchmarkROB01_TailRunVoids64K(b *testing.B)  { benchTailRun(b, 64<<10) }
func BenchmarkROB01_TailRunVoids512K(b *testing.B) { benchTailRun(b, 512<<10) }

func benchTailRun(b *testing.B, n int) {
	buf := voidTail(n)
	for b.Loop() {
		rel, _, _ := tailRun(buf, 0, int64(n))
		_ = metaEndsAt(buf, rel)
	}
}

// TestROB01_TailScanLinear: the header decodes the tail scan costs grow with its window, at most 16 per byte; the quadratic scan's grew with its square.
func TestROB01_TailScanLinear(t *testing.T) {
	var decodes int
	elemAtCount = &decodes
	defer func() { elemAtCount = nil }()
	for _, n := range []int{64 << 10, 512 << 10} {
		buf := voidTail(n)
		decodes = 0
		rel, _, ok := tailRun(buf, 0, int64(n))
		_ = metaEndsAt(buf, rel)
		if decodes > 16*n {
			t.Errorf("tail scan of %d KiB of Voids decoded %d element headers, past 16 per byte", n>>10, decodes)
		}
		if ok {
			t.Errorf("a tail ending on a stray byte tiles")
		}
	}
}
