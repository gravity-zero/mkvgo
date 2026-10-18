package mp4

import (
	"bytes"
	"strings"
	"testing"
)

// TestCENCSixteenByteIVLeavesCounterRoom: with a 16-byte 'cenc' IV the per-sample number sits in its first 8 bytes
// and AES-CTR counts blocks in the last 8; last bytes near 2^64 carried the count into the next sample's number,
// and two samples shared keystream. Such an IV is refused; one that leaves room passes.
func TestCENCSixteenByteIVLeavesCounterRoom(t *testing.T) {
	opts := func(low byte) *CENCOptions {
		iv := append(make([]byte, 8), bytes.Repeat([]byte{low}, 8)...)
		return &CENCOptions{Scheme: "cenc", Key: make([]byte, 16), KeyID: make([]byte, 16), IV: iv, InlineKey: true}
	}
	if err := opts(0xFF).validate(); err == nil || !strings.Contains(err.Error(), "ffffffff00000000") {
		t.Errorf("an IV ending in ff..ff: %v, want it refused, naming the bound", err)
	}
	if err := opts(0x7F).validate(); err != nil {
		t.Errorf("an IV ending in 7f..7f: %v, want it accepted", err)
	}
}
