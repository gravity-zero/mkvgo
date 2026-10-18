package subtitle

import (
	"runtime"
	"testing"
)

// TestPGSObjectDecodedOncePerDisplaySet: a composition may name one object many times, and each reference decoded
// it again into a bitmap of its own: 64 references to a 1024x1024 object of a few bytes held 256 MiB, 255 references
// to a 4096x4096 one 16 GiB. An object is decoded once per display set, its references sharing the bitmap.
func TestPGSObjectDecodedOncePerDisplaySet(t *testing.T) {
	objs := make([]synthObject, 64)
	for i := range objs {
		objs[i] = synthObject{id: 1, x: i, y: i}
	}
	set := concat(
		pgsPCS(1920, 1080, pgsEpochStart, 0, objs...),
		pgsPDS(0, palWhite),
		pgsODS(1, 1024, 1024, []byte{0, 0}),
		pgsEND(),
	)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	sets, err := NewPGSDecoder().Decode(set)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || len(sets[0].Objects) != 64 {
		t.Fatalf("%d display sets, want one placing the object 64 times", len(sets))
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 32<<20 {
		t.Errorf("64 references to one 1024x1024 object allocated %d MiB, want one bitmap's 4 MiB", alloc>>20)
	}
}

// TestPGSBlockPixelsBounded: distinct objects still each cost a bitmap; a block whose display sets would decode
// past four 4096x4096 objects is refused instead of allocated.
func TestPGSBlockPixelsBounded(t *testing.T) {
	var parts [][]byte
	objs := make([]synthObject, 5)
	for i := range objs {
		objs[i] = synthObject{id: uint16(i + 1)}
		parts = append(parts, pgsODS(uint16(i+1), 4096, 4096, []byte{0, 0}))
	}
	set := concat(append(append([][]byte{pgsPCS(1920, 1080, pgsEpochStart, 0, objs...), pgsPDS(0, palWhite)}, parts...), pgsEND())...)
	if _, err := NewPGSDecoder().Decode(set); err == nil {
		t.Error("five 4096x4096 objects in one block decoded, want the block refused")
	}
}
