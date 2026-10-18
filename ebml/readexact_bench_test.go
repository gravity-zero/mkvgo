package ebml

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// BenchmarkReadExactLarge reads a 300 MB element from a file, as an attachment's FileData is read: the allocations
// and the heap peak tell what growing the buffer as bytes arrive costs over an exact allocation.
func BenchmarkReadExactLarge(b *testing.B) {
	const size = 300 << 20
	path := filepath.Join(b.TempDir(), "elem")
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		f, err := os.Open(path)
		if err != nil {
			b.Fatal(err)
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		buf, err := readExact(f, size)
		runtime.ReadMemStats(&after)
		f.Close()
		if err != nil || len(buf) != size {
			b.Fatal(err)
		}
		b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc)/(1<<20), "MiB-allocated")
		b.ReportMetric(float64(after.HeapSys)/(1<<20), "MiB-heap-sys")
		_, _ = io.Discard.Write(buf[:1])
	}
}
