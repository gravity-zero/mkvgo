package mp4

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"testing"
)

// walkTopBoxes returns the top-level box types in order, and fails the test if
// the chain does not walk cleanly to the end of the file.
func walkTopBoxes(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for off := 0; off < len(b); {
		if off+8 > len(b) {
			t.Fatalf("box chain desyncs: %d trailing byte(s) at %d", len(b)-off, off)
		}
		size := int64(binary.BigEndian.Uint32(b[off : off+4]))
		typ := string(b[off+4 : off+8])
		if size == 1 {
			size = int64(binary.BigEndian.Uint64(b[off+8 : off+16]))
		}
		if size < 8 || off+int(size) > len(b) {
			t.Fatalf("box %q at %d has invalid size %d (file %d bytes)", typ, off, size, len(b))
		}
		types = append(types, typ)
		off += int(size)
	}
	return types
}

// TestMP4RetimeTracksAfterInterruptedRun: recovering from a run that was cut
// short between the append and the type flip.
//
// That interrupted state is by design readable - the original moov is still the
// live one at the head, the previous run's shifted moov is stranded at the tail
// - and the natural recovery is to re-run the repair. The scan must therefore
// target the FIRST moov (the one a forward walk, and mp4.Open, actually read),
// not the last: retiming the stranded one would shift an already shifted moov,
// append a third, and leave the head moov - the one players read - untouched,
// all while returning nil. Every stranded moov is retired too, so the file is
// left with exactly one live movie header.
func TestMP4RetimeTracksAfterInterruptedRun(t *testing.T) {
	ctx := context.Background()
	path := editShiftFixture(t, false)

	// Strand a shifted moov at the tail, exactly as a crash after the append
	// (and before the 4-byte flip) leaves it.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	lay, err := scanTopLevelLayout(f)
	if err != nil {
		t.Fatal(err)
	}
	moovRaw, err := readRange(f, lay.moovOff, lay.moovSize)
	if err != nil {
		t.Fatal(err)
	}
	stranded, err := rewriteMoovEditShifts(moovRaw[lay.moovHdr:], map[uint64]int64{2: 900_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(stranded); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// The interrupted file keeps the ORIGINAL semantics: no delay yet.
	if got := measuredAudioDelayMs(t, path); got != 0 {
		t.Fatalf("an interrupted run must keep the original semantics, measured %dms", got)
	}

	// Recover: re-run the repair.
	if err := RetimeTracks(ctx, path, map[uint64]int64{2: 300_000_000}); err != nil {
		t.Fatalf("re-running the repair after an interrupted run: %v", err)
	}

	if got := measuredAudioDelayMs(t, path); got != 300 {
		t.Errorf("delay after recovery = %dms, want 300: the re-run must retime the live head moov "+
			"(a stranded moov must not become the live one, nor be retimed twice)", got)
	}

	types := walkTopBoxes(t, path)
	moovs := 0
	for _, typ := range types {
		if typ == "moov" {
			moovs++
		}
	}
	if moovs != 1 {
		t.Errorf("top-level boxes = %v: want exactly 1 live moov, got %d", types, moovs)
	}
	if types[len(types)-1] != "moov" {
		t.Errorf("top-level boxes = %v: the live moov must be the appended one (last)", types)
	}
}

// TestMP4RetimeTracksRefusesDestructiveLayouts: the two layouts on which the
// append-and-retire landing would destroy the file rather than repair it. Both
// used to be walked right past, the moov appended, the old one retired - and
// nil returned on a file with no reachable movie header.
func TestMP4RetimeTracksRefusesDestructiveLayouts(t *testing.T) {
	ctx := context.Background()

	t.Run("last box runs to EOF", func(t *testing.T) {
		path := editShiftFixture(t, false)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Re-declare the last top-level box with size 0 (= to end of file), the
		// form streaming writers emit. Appending a moov would extend it.
		var lastOff int
		for off := 0; off < len(b); {
			size := int64(binary.BigEndian.Uint32(b[off : off+4]))
			if size == 1 {
				size = int64(binary.BigEndian.Uint64(b[off+8 : off+16]))
			}
			lastOff = off
			off += int(size)
		}
		binary.BigEndian.PutUint32(b[lastOff:lastOff+4], 0)
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}

		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := RetimeTracks(ctx, path, map[uint64]int64{2: 300_000_000}); err == nil {
			t.Error("a to-EOF last box must be refused: the appended moov would land inside it")
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) {
			t.Errorf("a refused repair must not write (size %d -> %d)", len(before), len(after))
		}
	})

	t.Run("junk past the last box", func(t *testing.T) {
		path := editShiftFixture(t, false)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte{0xDE, 0xAD, 0xBE}); err != nil { // < 8: no box can start here
			t.Fatal(err)
		}
		f.Close()

		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := RetimeTracks(ctx, path, map[uint64]int64{2: 300_000_000}); err == nil {
			t.Error("junk past the last box must be refused: the appended moov would sit behind it")
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) {
			t.Errorf("a refused repair must not write (size %d -> %d)", len(before), len(after))
		}
	})
}
