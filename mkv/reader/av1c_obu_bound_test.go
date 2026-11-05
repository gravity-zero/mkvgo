package reader

import (
	"reflect"
	"testing"
)

// TestAV1COBUSizeDoesNotWrap: an av1C OBU's leb128 size was converted to int before its bound check, so on 32-bit
// 0x7FFFFFFF made i+size wrap (a slice panic) and 2^32+2 truncated to 2 (two bytes read as a sequence header).
func TestAV1COBUSizeDoesNotWrap(t *testing.T) {
	head := []byte{0x81, 0x00, 0x0C, 0x00} // av1C v1, Main profile, 8-bit 4:2:0
	want := av1Colour(head)
	for name, size := range map[string][]byte{
		"0x7FFFFFFF": {0xFF, 0xFF, 0xFF, 0xFF, 0x07},
		"2^32+2":     {0x82, 0x80, 0x80, 0x80, 0x10},
	} {
		cp := append(append(append([]byte{}, head...), 0x0A), size...) // OBU_SEQUENCE_HEADER with a size field
		cp = append(cp, 0x00, 0x00)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: av1Colour panicked: %v", name, r)
				}
			}()
			if got := av1Colour(cp); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: an OBU sized past its av1C was read: %+v", name, got)
			}
		}()
	}
}
