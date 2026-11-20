package subtitle

import (
	"runtime"
	"strings"
	"testing"
)

// A full-size region shown again under a CLUT that changes each time made a picture of 64 MiB per display set:
// 254 bytes of block allocated 208 MiB. The block's budget is the region and one picture of it.
func TestDVBSubBlockPicturesBounded(t *testing.T) {
	entry := func(y byte) [6]byte { return [6]byte{1, 0xe1, y, 0x80, 0x80, 0x00} }
	block := concat(dvbPCS(2, dvbPlacement{}), dvbRCS(0, true, dvbMaxDim, dvbMaxDim, 3, 0, 1), dvbCDS(0, entry(0x40)), dvbEDS())
	for i := 0; i < 6; i++ {
		again := dvbCDS(0, entry(byte(0x50+i)))
		again[7] = byte(i+1) << 4 // CLUT version
		block = concat(block, dvbPCS(0, dvbPlacement{}), again, dvbEDS())
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := newDVBSub(t).Decode(block)
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "fills and draws more than") {
		t.Fatalf("err = %v, want the block's pixel bound", err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 100<<20 {
		t.Errorf("a %d-byte block allocated %d MiB, want the region and one picture, 80 MiB", len(block), alloc>>20)
	}
}
