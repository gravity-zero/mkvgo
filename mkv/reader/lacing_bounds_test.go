package reader

import "testing"

// TestEBMLLacingSizesStayInTheData: EBML lace diffs whose running total wraps past 2^63 back onto the payload's
// length are refused by the size decoder itself, and so is a size VINT whose first byte is zero.
func TestEBMLLacingSizesStayInTheData(t *testing.T) {
	raw := []byte{0x80} // first frame: 0 bytes
	for i := 0; i < 23; i++ {
		raw = append(raw, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFE) // diff of about +2^55
	}
	raw = append(raw, make([]byte, 100)...)
	if sizes, _, err := decodeLacingSizes(lacingEBML, raw, 25); err == nil {
		t.Errorf("25 frames of up to %d bytes accepted in a %d-byte payload", sizes[24], len(raw))
	}
	if _, _, err := decodeLacingSizes(lacingEBML, []byte{0x00, 0x81, 1, 2, 3}, 3); err == nil {
		t.Error("a lace size VINT starting with a zero byte was accepted")
	}
}
