package reader

import (
	"bytes"
	"testing"
)

// TestEBMLLaceOfOneFrame: RFC 9559 codes the size of every frame but the last, so a lace of one frame codes none and
// its payload is that frame. The full decoder read the frame's first bytes as a size and cut the frame short; the
// header-only one refused the block. Both read the whole payload as the frame now.
func TestEBMLLaceOfOneFrame(t *testing.T) {
	payload := []byte{0x85, 1, 2, 3, 4, 5, 6}
	sizes, hdr, err := decodeLacingSizes(lacingEBML, payload, 1)
	if err != nil || hdr != 0 || len(sizes) != 1 || sizes[0] != len(payload) {
		t.Errorf("full decoder: sizes %v, header %d, %v; want [%d] and 0", sizes, hdr, err, len(payload))
	}
	sizes, hdr, err = readLaceHeader(bytes.NewReader(payload), lacingEBML, 1, int64(len(payload)))
	if err != nil || hdr != 0 || len(sizes) != 1 || sizes[0] != len(payload) {
		t.Errorf("header-only decoder: sizes %v, header %d, %v; want [%d] and 0", sizes, hdr, err, len(payload))
	}
}
