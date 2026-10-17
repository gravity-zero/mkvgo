package reader

import (
	"sync/atomic"
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

// TestROB01_TailScanLinear: at most 16 header decodes per byte, the same per byte at 64 and 512 KiB; the quadratic scan's grew with the window.
func TestROB01_TailScanLinear(t *testing.T) {
	var decodes atomic.Int64
	elemAtCount.Store(&decodes)
	defer elemAtCount.Store(nil)
	perByte := make([]float64, 0, 2)
	for _, n := range []int{64 << 10, 512 << 10} {
		buf := voidTail(n)
		decodes.Store(0)
		rel, _, ok := tailRun(buf, 0, int64(n))
		_ = metaEndsAt(buf, rel)
		d := decodes.Load()
		if d > 16*int64(n) {
			t.Errorf("tail scan of %d KiB of Voids decoded %d element headers, past 16 per byte", n>>10, d)
		}
		if ok {
			t.Errorf("a tail ending on a stray byte tiles")
		}
		perByte = append(perByte, float64(d)/float64(n))
	}
	if perByte[1] > 1.5*perByte[0] {
		t.Errorf("decodes per byte grow from %.2f at 64 KiB to %.2f at 512 KiB: not linear", perByte[0], perByte[1])
	}
}
