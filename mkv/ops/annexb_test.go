package ops

import "testing"

// TestAnnexBNALLengthPastTheFrame: a 4-byte NAL length of 0x7FFFFFFF wrapped pos+n negative on 32-bit, passed the
// bound check and panicked on the slice; it is refused on every architecture.
func TestAnnexBNALLengthPastTheFrame(t *testing.T) {
	frame := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x7F, 0xFF, 0xFF, 0xFF, 0x41, 0x42}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	if _, err := annexBFromLengthPrefixed(frame, 4); err == nil {
		t.Error("a NAL length past the frame was accepted")
	}
}
