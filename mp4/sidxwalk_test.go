package mp4

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestSidxBoxesWalkedOneAtATime: every sidx ahead of the fragments was read and kept before the first was looked at,
// each up to 786 KiB, so a file stacking thousands of them held gigabytes on a head-only probe. They are handed over
// one at a time, and the walk stops at the one that serves.
func TestSidxBoxesWalkedOneAtATime(t *testing.T) {
	var f bytes.Buffer
	for i := 0; i < 10; i++ {
		b := make([]byte, 8+12)
		binary.BigEndian.PutUint32(b, uint32(len(b)))
		copy(b[4:], "sidx")
		f.Write(b)
	}
	visits := 0
	walkSidxBoxes(bytes.NewReader(f.Bytes()), int64(f.Len()), func([]byte) bool { visits++; return visits == 2 })
	if visits != 2 {
		t.Errorf("%d sidx boxes read, want the walk to stop at the second", visits)
	}
}
