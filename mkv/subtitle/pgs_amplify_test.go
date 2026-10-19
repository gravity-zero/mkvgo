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

// TestPGSEpochObjectsBounded: objects live until an epoch start, so a stream that never starts one (or a block
// inflated from a few KiB) piled up every object it defined; an epoch holding past pgsMaxEpochBytes is refused.
func TestPGSEpochObjectsBounded(t *testing.T) {
	d := NewPGSDecoder()
	rle := make([]byte, 60_000)
	for i := range 1000 {
		if _, err := d.Decode(pgsODS(uint16(i), 64, 64, rle)); err != nil {
			if i*len(rle) < pgsMaxEpochBytes-len(rle) {
				t.Fatalf("refused after %d bytes, under the %d-byte budget: %v", i*len(rle), pgsMaxEpochBytes, err)
			}
			return
		}
	}
	t.Errorf("an epoch kept 1000 objects of %d bytes, want a refusal past %d bytes", len(rle), pgsMaxEpochBytes)
}

// TestPGSEpochObjectRedefinedCountsOnce: redefining an object replaces it, its old bytes leave the budget.
func TestPGSEpochObjectRedefinedCountsOnce(t *testing.T) {
	d := NewPGSDecoder()
	rle := make([]byte, 60_000)
	for range 1000 {
		if _, err := d.Decode(pgsODS(1, 64, 64, rle)); err != nil {
			t.Fatalf("one object redefined over and over was refused: %v", err)
		}
	}
}

// TestPGSEpochRefusalLeavesTheBudgetAsItWas: an object refused for the budget is not counted, so a caller that skips
// the block can still define one that fits.
func TestPGSEpochRefusalLeavesTheBudgetAsItWas(t *testing.T) {
	d := NewPGSDecoder()
	rle := make([]byte, 60_000)
	n := 0
	for ; n < 1000; n++ {
		if _, err := d.Decode(pgsODS(uint16(n), 64, 64, rle)); err != nil {
			break
		}
	}
	if _, err := d.Decode(pgsODS(uint16(n), 64, 64, rle)); err == nil {
		t.Fatal("the same object fit after its refusal")
	}
	if _, err := d.Decode(pgsODS(uint16(n+1), 64, 64, make([]byte, 1000))); err != nil {
		t.Errorf("an object that fits was refused after a refusal: %v", err)
	}
}
