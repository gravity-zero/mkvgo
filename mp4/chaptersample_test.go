package mp4

import (
	"bytes"
	"runtime"
	"testing"
)

// TestChapterTitleSampleBounded: a chapter track's sample is a title of a few bytes, but its buffer was sized from
// the sample size the file declares, before any byte was read: one sample claiming 4 GiB of a sparse or remote file
// allocated 4 GiB on a metadata probe. A sample larger than a title can be is not read as one.
func TestChapterTitleSampleBounded(t *testing.T) {
	samples := []inSample{{offset: 0, size: 128 << 20}}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	readChapterTrack(bytes.NewReader(make([]byte, 64)), samples)
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Errorf("a 128 MiB chapter sample allocated %d MiB before its bytes were read", alloc>>20)
	}
}
