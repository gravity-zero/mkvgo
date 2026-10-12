//go:build linux

package ops

// Batch vs per-call keyframe extraction, on the same selection.
//
// Synthetic (CI-safe):
//
//	go test ./mkv/ops/ -run '^$' -bench 'Keyframes' -benchmem
//
// Real files (opt-in, prints one row per file x mode x cache state):
//
//	MKVGO_KF_FILES="a.mkv:b.mkv" MKVGO_KF_EVERY_MS=7000 MKVGO_KF_COLD=1 \
//	  go test ./mkv/ops/ -run TestKeyframesRealFileReport -v -count=1
//
// MKVGO_KF_COLD=1 drops the file's pages from the page cache before each
// cold run (posix_fadvise DONTNEED, Linux; no root needed) - on a network
// mount it only drops the local cache, the server's stays warm.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

func benchFixture(b *testing.B) (string, []int64) {
	b.Helper()
	big := lp(append([]byte{0x65, 0x88}, make([]byte, 120<<10)...))
	p := lp(append([]byte{0x41, 0xC0}, make([]byte, 8<<10)...))
	fx := h264Fixture(func(i int) []byte {
		if i%25 == 0 {
			return big
		}
		return p
	})
	fx.clusters = 300
	src := fx.write(b)
	var targets []int64
	for ms := int64(0); ms < 300_000; ms += 7000 {
		targets = append(targets, ms)
	}
	return src, targets
}

func BenchmarkKeyframesBatch(b *testing.B) {
	src, targets := benchFixture(b)
	fs, io := countingTestFS()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ExtractKeyframes(context.Background(), src, KeyframeSelect{TargetsMs: targets}, func(*KeyframeBatchSample) error { return nil }, mkv.Options{FS: fs}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(io.bytes)/float64(b.N), "readB/op")
	b.ReportMetric(float64(io.reads)/float64(b.N), "reads/op")
	b.ReportMetric(float64(io.seeks)/float64(b.N), "seeks/op")
}

func BenchmarkKeyframesSingleLoop(b *testing.B) {
	src, targets := benchFixture(b)
	fs, io := countingTestFS()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, ms := range targets {
			if _, err := ExtractKeyframeSample(context.Background(), src, ms, mkv.Options{FS: fs}); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(io.bytes)/float64(b.N), "readB/op")
	b.ReportMetric(float64(io.reads)/float64(b.N), "reads/op")
	b.ReportMetric(float64(io.seeks)/float64(b.N), "seeks/op")
}

// ioTally counts every read and repositioning seek through the FS it backs,
// metadata included, so both extraction styles are measured alike.
type ioTally struct{ bytes, reads, seeks, opens int64 }

type tallyFile struct {
	*os.File
	t   *ioTally
	off int64
}

func (f *tallyFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	f.t.reads++
	f.t.bytes += int64(n)
	f.off += int64(n)
	return n, err
}

func (f *tallyFile) Seek(off int64, whence int) (int64, error) {
	n, err := f.File.Seek(off, whence)
	if err == nil && n != f.off {
		f.t.seeks++
		f.off = n
	}
	return n, err
}

func countingTestFS() (*mkv.FS, *ioTally) {
	t := &ioTally{}
	return &mkv.FS{Open: func(path string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		t.opens++
		return &tallyFile{File: f, t: t}, nil
	}}, t
}

func dropPageCache(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	const fadvDontNeed = 4
	_, _, errno := syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), 0, 0, fadvDontNeed, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func TestKeyframesRealFileReport(t *testing.T) {
	files := os.Getenv("MKVGO_KF_FILES")
	if files == "" {
		t.Skip("set MKVGO_KF_FILES (colon-separated) to run on real files")
	}
	every := int64(7000)
	if v, err := strconv.ParseInt(os.Getenv("MKVGO_KF_EVERY_MS"), 10, 64); err == nil && v > 0 {
		every = v
	}
	states := []string{"warm"}
	if os.Getenv("MKVGO_KF_COLD") == "1" {
		states = []string{"cold", "warm"}
	}
	ctx := context.Background()
	t.Logf("%-14s %-6s %-5s %6s %9s %9s %11s %7s %7s %6s %10s %8s", "file", "mode", "cache", "frames", "wall", "cpu", "readB", "reads", "seeks", "opens", "allocB", "mallocs")
	for _, path := range strings.Split(files, ":") {
		// The selection both modes extract: the batch's own, as targets.
		st, err := ExtractKeyframes(ctx, path, KeyframeSelect{EveryMs: every}, func(*KeyframeBatchSample) error { return nil })
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		targets := st.PtsMs
		for _, mode := range []string{"single", "batch"} {
			for _, state := range states {
				if state == "cold" {
					if err := dropPageCache(path); err != nil {
						t.Fatalf("drop cache: %v", err)
					}
				}
				fs, io := countingTestFS()
				var ms0, ms1 runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&ms0)
				c0, t0 := cpuTime(), time.Now()
				var sink int
				switch mode {
				case "single":
					for _, at := range targets {
						ks, err := ExtractKeyframeSample(ctx, path, at, mkv.Options{FS: fs})
						if err != nil {
							t.Fatal(err)
						}
						sink += len(ks.Data)
					}
				case "batch":
					if _, err := ExtractKeyframes(ctx, path, KeyframeSelect{TargetsMs: targets}, func(s *KeyframeBatchSample) error {
						sink += len(s.Data)
						return nil
					}, mkv.Options{FS: fs}); err != nil {
						t.Fatal(err)
					}
				}
				wall, cpu := time.Since(t0), cpuTime()-c0
				runtime.ReadMemStats(&ms1)
				t.Logf("%-14s %-6s %-5s %6d %9s %9s %11d %7d %7d %6d %10d %8d", trimName(path), mode, state, len(targets),
					wall.Round(time.Millisecond), cpu.Round(time.Millisecond), io.bytes, io.reads, io.seeks, io.opens,
					ms1.TotalAlloc-ms0.TotalAlloc, ms1.Mallocs-ms0.Mallocs)
				_ = sink
			}
		}
	}
}

func trimName(p string) string {
	b := filepath.Base(p)
	if len(b) > 14 {
		return b[:14]
	}
	return b
}
