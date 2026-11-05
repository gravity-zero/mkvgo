package writer

import (
	"bytes"
	"io"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestBlockGroupWritesTheFrameWithoutACopy: a BlockGroup was assembled in a buffer before it was written, the frame
// copied into it - for a source with additions on every frame (HDR10+), a copy of the whole stream. The frame goes
// to the writer as it is; what the group allocates is its own elements.
func TestBlockGroupWritesTheFrameWithoutACopy(t *testing.T) {
	frame := make([]byte, 1<<20)
	adds := []mkv.BlockAddition{{ID: 4, Data: bytes.Repeat([]byte{0xB5}, 64)}}
	ref := int64(-40)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range 20 {
		if err := writeBlockGroup(io.Discard, 1, 0, frame, 40, &ref, 0, adds); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > 20*64<<10 {
		t.Errorf("20 BlockGroups of a 1 MiB frame allocated %d bytes, want the frames written, not copied", got)
	}
}
