package mp4

import (
	"bytes"
	"testing"
)

// TestAV1OBUSizePast2To32IsRefused: an OBU whose obu_size is 2^32+3 was taken as int before the bounds check; on a
// 32-bit platform it wrapped to 3 and passed.
func TestAV1OBUSizePast2To32IsRefused(t *testing.T) {
	// OBU_PADDING (type 15) with has_size, obu_size 2^32+3 as LEB128, then 3 bytes.
	sample := []byte{15<<3 | 1<<1, 0x83, 0x80, 0x80, 0x80, 0x10, 1, 2, 3}
	if subs, err := splitAV1Subsamples(sample); err == nil {
		t.Errorf("an OBU of 2^32+3 bytes in a %d-byte sample was split into %v", len(sample), subs)
	}
}

// TestVP9SuperframeSizesDoNotWrap: a superframe of a 1-byte frame then one of 2^31-1 bytes: the second end, summed as
// a 32-bit int, wrapped negative and passed the bounds check.
func TestVP9SuperframeSizesDoNotWrap(t *testing.T) {
	marker := byte(0xC0 | 3<<3 | 1) // 4 bytes a size, 2 frames
	sample := append(bytes.Repeat([]byte{0x82}, 8), marker, 1, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0x7F, marker)
	if frames, _, err := vp9SplitSuperframe(sample); err == nil {
		t.Errorf("a superframe index of 1 + 2^31-1 bytes in a %d-byte sample gave %v", len(sample), frames)
	}
}
