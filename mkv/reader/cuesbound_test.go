package reader

import (
	"bytes"
	"context"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestCuesAllocationBounded: a Cues element of empty CuePoints (2 bytes each) costs nothing in memory, and one past
// maxCuePoints points is refused instead of holding them all.
func TestCuesAllocationBounded(t *testing.T) {
	cues := bytes.Repeat([]byte{0xBB, 0x80}, 4<<20) // 8 MiB, 4M empty points
	p := &parser{r: bytes.NewReader(cues), metaBudget: maxMetadataBytes, ctx: context.Background()}
	var c mkv.Container
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := p.parseCuesFrom(int64(len(cues)), &c); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 32<<20 || len(c.Cues) != 0 {
		t.Errorf("8 MiB of empty CuePoints: %d kept, %d MiB allocated", len(c.Cues), grew>>20)
	}

	defer func(n int) { maxCuePoints = n }(maxCuePoints)
	maxCuePoints = 3
	point := []byte{0xBB, 0x8B, 0xB3, 0x81, 0x01, 0xB7, 0x86, 0xF7, 0x81, 0x01, 0xF1, 0x81, 0x00}
	p = &parser{r: bytes.NewReader(bytes.Repeat(point, 4)), metaBudget: maxMetadataBytes, ctx: context.Background()}
	c = mkv.Container{}
	if err := p.parseCuesFrom(int64(4*len(point)), &c); err == nil {
		t.Errorf("4 CuePoints past a bound of 3 were all kept (%d)", len(c.Cues))
	}
}
