package subtitle

import (
	"runtime"
	"testing"
)

// TestPGSObjectBufferFollowsItsData: an object's buffer was sized by the length its first segment declares, before
// any of it had arrived: 14 bytes of block cleared 4 MiB, a hundred times over in a 1.4 KB block.
func TestPGSObjectBufferFollowsItsData(t *testing.T) {
	var block []byte
	for i := 0; i < 100; i++ {
		block = append(block, pgsSegment(pgsSegODS, []byte{0x00, 0x01, 0x00, 0x80, 0x40, 0x00, 0x04, 0x00, 0x01, 0x00, 0x01})...)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _ = NewPGSDecoder().Decode(block)
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > 4<<20 {
		t.Errorf("decoding %d bytes allocated %d MiB", len(block), got>>20)
	}
}
