package ebml

import (
	"bytes"
	"testing"
)

func TestROB03_ReadFloatZeroLength(t *testing.T) {
	v, err := ReadFloat(bytes.NewReader([]byte{0x40}), 0)
	if err != nil || v != 0 {
		t.Fatalf("ReadFloat(size 0) = %v, %v; want 0, nil (RFC 8794: an empty float is 0)", v, err)
	}
	for _, size := range []int64{1, 2, 3, 5, 7, 9, -1} {
		if _, err := ReadFloat(bytes.NewReader(make([]byte, 16)), size); err == nil {
			t.Errorf("ReadFloat(size %d) accepted", size)
		}
	}
}
