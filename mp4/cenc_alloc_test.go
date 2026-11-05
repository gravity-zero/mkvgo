package mp4

import (
	"context"
	"os"
	"runtime"
	"testing"
)

// TestCENCFullPassAllocatesNoSegmentCopies: a CENC full pass read each segment into a fresh buffer, encrypted a copy
// of it and joined head and copy into a third - three segment-sized allocations a segment, where the clear pass has
// none, 4.4 times this source. One buffer a track now, grown to its largest segment (twice that at most), keeps what
// CENC adds over the clear pass under twice the source here, and constant on a longer one.
func TestCENCFullPassAllocatesNoSegmentCopies(t *testing.T) {
	const src = "../internal/testdata/slices.mp4"
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	alloc := func(o Options) uint64 {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		if err := RemuxToHLS(context.Background(), src, t.TempDir(), o); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	clear := alloc(Options{SegmentMs: 1000, NoIFrames: true})
	cbcs := alloc(Options{SegmentMs: 1000, NoIFrames: true, CENC: &CENCOptions{Scheme: "cbcs", Key: cencKey, KeyID: cencKID, IV: cencIVFor("cbcs"), InlineKey: true}})
	if extra := int64(cbcs) - int64(clear); extra > 2*st.Size() {
		t.Errorf("cbcs allocated %d bytes more than the clear pass, for %d bytes of source", extra, st.Size())
	}
}
