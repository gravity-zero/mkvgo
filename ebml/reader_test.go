package ebml

import (
	"bytes"
	"testing"
)

func TestReadVINT_EmptyInput(t *testing.T) {
	r := bytes.NewReader(nil)
	_, _, err := ReadVINT(r)
	if err == nil {
		t.Fatal("expected error on empty input")
	}
}

func TestReadVINT_ZeroByte(t *testing.T) {
	r := bytes.NewReader([]byte{0x00})
	_, _, err := ReadVINT(r)
	if err == nil {
		t.Fatal("expected error on zero byte")
	}
}

func TestReadVINT_TruncatedMultiByte(t *testing.T) {
	// 0x40 means 2-byte VINT, but only 1 byte provided
	r := bytes.NewReader([]byte{0x40})
	_, _, err := ReadVINT(r)
	if err == nil {
		t.Fatal("expected error on truncated multi-byte VINT")
	}
}

func TestReadDataSize_UnknownSize(t *testing.T) {
	// 1-byte unknown: 0xFF means all-ones VINT => unknown
	r := bytes.NewReader([]byte{0xFF})
	size, n, err := ReadDataSize(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != -1 {
		t.Errorf("expected -1 for unknown size, got %d", size)
	}
	if n != 1 {
		t.Errorf("expected 1 byte consumed, got %d", n)
	}
}

func TestReadDataSize_Error(t *testing.T) {
	r := bytes.NewReader(nil)
	_, _, err := ReadDataSize(r)
	if err == nil {
		t.Fatal("expected error on empty input")
	}
}

func TestReadElementID_Error(t *testing.T) {
	r := bytes.NewReader(nil)
	_, _, err := ReadElementID(r)
	if err == nil {
		t.Fatal("expected error on empty input")
	}
}

func TestReadElementHeader_TruncatedSize(t *testing.T) {
	// Valid 1-byte ID (0x86) but no data-size bytes following
	r := bytes.NewReader([]byte{0x86})
	_, _, err := ReadElementHeader(r)
	if err == nil {
		t.Fatal("expected error on truncated header")
	}
}

func TestReadElementHeader_EmptyInput(t *testing.T) {
	r := bytes.NewReader(nil)
	_, _, err := ReadElementHeader(r)
	if err == nil {
		t.Fatal("expected error on empty input")
	}
}

func TestReadVINT_MultiByteValues(t *testing.T) {
	for _, tt := range []struct {
		input []byte
		val   uint64
		width int
	}{
		{[]byte{0x81}, 0x81, 1},
		{[]byte{0x40, 0x01}, 0x4001, 2},
		{[]byte{0x20, 0x00, 0x01}, 0x200001, 3},
	} {
		r := bytes.NewReader(tt.input)
		val, n, err := ReadVINT(r)
		if err != nil {
			t.Errorf("ReadVINT(%x) error: %v", tt.input, err)
			continue
		}
		if val != tt.val {
			t.Errorf("ReadVINT(%x) val = 0x%X, want 0x%X", tt.input, val, tt.val)
		}
		if n != tt.width {
			t.Errorf("ReadVINT(%x) width = %d, want %d", tt.input, n, tt.width)
		}
	}
}
