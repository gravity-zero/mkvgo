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

// TestPGSBlockPicturesAreBudgetedToTwoPlanes: a block of a hundred bytes naming four 4096x4096 objects with no
// data handed back four 64 MiB pictures. A block's pictures share two planes, as its composites do.
func TestPGSBlockPicturesAreBudgetedToTwoPlanes(t *testing.T) {
	pcs := []byte{0x10, 0x00, 0x10, 0x00, 0x10, 0x00, 0x00, 0x80, 0x00, 0x00, 0x04}
	var ods []byte
	for id := byte(1); id <= 4; id++ {
		pcs = append(pcs, 0x00, id, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
		ods = append(ods, pgsSegment(pgsSegODS, []byte{0x00, id, 0x00, 0xc0, 0x00, 0x00, 0x04, 0x10, 0x00, 0x10, 0x00})...)
	}
	block := concat(pgsSegment(pgsSegPCS, pcs), pgsSegment(pgsSegPDS, []byte{0x00, 0x00, 0x01, 235, 128, 128, 255}), ods, pgsEND())
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _ = NewPGSDecoder().Decode(block)
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > 140<<20 {
		t.Errorf("decoding %d bytes allocated %d MiB, past the two planes a block may hand back", len(block), got>>20)
	}
}
