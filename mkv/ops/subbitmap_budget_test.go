package ops

import (
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// pgsFarCorners is one PGS block: a display set defining two 4x2 objects at opposite corners of a 4096x4096 plane,
// then again as many compositions of the same two objects, each a few dozen bytes.
func pgsFarCorners(again int) []byte {
	pcs := func(state byte) []byte {
		return pgsSeg(0x16, []byte{0x10, 0x00, 0x10, 0x00, 0x10, 0x00, 0x00, state, 0x00, 0x00, 0x02,
			0x00, 0x0a, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x0b, 0x00, 0x00, 0x0f, 0xf0, 0x0f, 0xf0})
	}
	out := pcs(0x80)
	out = append(out, pgsSeg(0x14, []byte{0x00, 0x00, 0x01, 235, 128, 128, 255})...)
	rle := []byte{0x00, 0x84, 0x01, 0x00, 0x00, 0x00, 0x00}
	for _, id := range []byte{0x0a, 0x0b} {
		ods := []byte{0x00, id, 0x00, 0xc0, 0x00, 0x00, byte(len(rle) + 4), 0x00, 0x04, 0x00, 0x02}
		out = append(out, pgsSeg(0x15, append(ods, rle...))...)
	}
	out = append(out, pgsSeg(0x80, nil)...)
	for i := 0; i < again; i++ {
		out = append(out, pcs(0x00)...)
		out = append(out, pgsSeg(0x80, nil)...)
	}
	return out
}

// TestBitmapBlockCompositesAreBudgeted: every composition of a block was flattened onto its own picture, as large
// as the objects are far apart, and all of them held until the block was done: 64 MiB per 30 bytes of block. A
// block whose compositions together cover more than a subtitle plane holds is refused as the damage it is.
func TestBitmapBlockCompositesAreBudgeted(t *testing.T) {
	track := subtitleTrack(2, "pgs")
	alloc := func(again int) (uint64, int) {
		cues := 0
		a, err := newBitmapAssembler(func(BitmapCue) error { cues++; return nil }, &track, nil)
		if err != nil {
			t.Fatalf("assembler: %v", err)
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		if err := a.block(mkv.Block{TrackNumber: 2, Timecode: 0, Data: pgsFarCorners(again)}); err != nil {
			t.Fatalf("block with %d more compositions: %v", again, err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc, cues
	}
	one, _ := alloc(0)
	if one < 32<<20 {
		t.Fatalf("one composition allocated %d bytes: the fixture no longer spreads its objects, this test proves nothing", one)
	}
	if six, _ := alloc(5); six > 2*one+(4<<20) { // two planes at most, whatever the block names again
		t.Errorf("six compositions in one block allocated %d MiB, one %d MiB: the block's compositions are not budgeted together", six>>20, one>>20)
	}
}

// TestSubtitleIndexNilMarshal: a nil index says so.
func TestSubtitleIndexNilMarshal(t *testing.T) {
	var ix *SubtitleIndex
	if _, err := ix.MarshalBinary(); err == nil {
		t.Error("MarshalBinary of a nil index returned no error")
	}
}
