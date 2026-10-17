package reader

import (
	"testing"
	"time"
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

// TestROB01_TailScanLinear: the quadratic scan took 12 s on 64 KiB of Voids, the linear one about 1 ms; 1 s holds on a loaded CI runner.
func TestROB01_TailScanLinear(t *testing.T) {
	for _, n := range []int{64 << 10, 512 << 10} {
		buf := voidTail(n)
		start := time.Now()
		rel, _, ok := tailRun(buf, 0, int64(n))
		_ = metaEndsAt(buf, rel)
		if d := time.Since(start); d > time.Second {
			t.Errorf("tail scan of %d KiB of Voids took %v", n>>10, d)
		}
		if ok {
			t.Errorf("a tail ending on a stray byte tiles")
		}
	}
}
