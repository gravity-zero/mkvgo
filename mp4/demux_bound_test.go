package mp4

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"testing"
)

// TestTrackWindowsRefuseASamplePastTheSource: a fragment's trun can declare a 4 GiB sample in a small file; the
// read is refused before the buffer for it is allocated.
func TestTrackWindowsRefuseASamplePastTheSource(t *testing.T) {
	src := bytes.NewReader(make([]byte, 1<<10))
	sr := newTrackWindows(src, 1<<10, 1)
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	_, err := sr.read(0, []inSample{{offset: 100, size: 0xFFFFFFF0}}, 0)
	runtime.ReadMemStats(&m1)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a sample past the source: %v", err)
	}
	if alloc := m1.TotalAlloc - m0.TotalAlloc; alloc > 1<<20 {
		t.Errorf("allocated %d bytes for a sample the source does not hold", alloc)
	}
}

// TestTrackWindowsRefuseASamplePastTheCap: a table may declare a 4 GiB sample a large file physically holds; the
// read is refused before the buffer for it is allocated, as a segment of that size is.
func TestTrackWindowsRefuseASamplePastTheCap(t *testing.T) {
	sr := newTrackWindows(bytes.NewReader(make([]byte, 1<<10)), 1<<40, 1)
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	_, err := sr.read(0, []inSample{{offset: 0, size: 1 << 29}}, 0)
	runtime.ReadMemStats(&m1)
	if err == nil || errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a 512 MiB sample: %v, want a refusal naming the cap", err)
	}
	if alloc := m1.TotalAlloc - m0.TotalAlloc; alloc > 1<<20 {
		t.Errorf("allocated %d bytes for a sample past the cap", alloc)
	}
}
