package mp4

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// TestLazyMoovNestingBounded: the metadata path's moov reader recursed into every container box whatever its parent,
// with no depth bound: millions of nested trak boxes, sixty megabytes of them, overflow the goroutine stack, a fatal
// error no recover catches. Nesting past the sample tables' depth is refused.
func TestLazyMoovNestingBounded(t *testing.T) {
	payload := []byte{}
	for i := 0; i < 20; i++ {
		box := make([]byte, 8, 8+len(payload))
		binary.BigEndian.PutUint32(box, uint32(8+len(payload)))
		copy(box[4:], "trak")
		payload = append(box, payload...)
	}
	_, err := readMoovLazyAt(bytes.NewReader(payload), 0, int64(len(payload)))
	if err == nil || !strings.Contains(err.Error(), "deeper than") {
		t.Errorf("20 nested trak boxes: %v, want the nesting refused", err)
	}
}
