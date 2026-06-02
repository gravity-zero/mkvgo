package ebml

import (
	"bytes"
	"strings"
	"testing"
)

func TestReadUint_InvalidSize(t *testing.T) {
	r := bytes.NewReader([]byte{0x01})
	for _, size := range []int64{-1, 0, 9, 100} {
		_, err := ReadUint(r, size)
		if size == 0 {
			// size 0 => ReadFull with 0 bytes => no error, returns 0
			if err != nil {
				t.Errorf("ReadUint(size=%d) unexpected error: %v", size, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("ReadUint(size=%d) expected error, got nil", size)
		}
	}
}

func TestReadUint_ReadError(t *testing.T) {
	// 2-byte uint but only 1 byte available
	r := bytes.NewReader([]byte{0x01})
	_, err := ReadUint(r, 2)
	if err == nil {
		t.Fatal("expected read error")
	}
}

func TestReadFloat_InvalidSize(t *testing.T) {
	r := bytes.NewReader(make([]byte, 16))
	for _, size := range []int64{0, 1, 3, 5, 7, 16} {
		_, err := ReadFloat(r, size)
		if err == nil {
			t.Errorf("ReadFloat(size=%d) expected error, got nil", size)
		}
	}
}

func TestReadFloat_ReadError(t *testing.T) {
	// 4-byte float but only 2 bytes
	r := bytes.NewReader([]byte{0x01, 0x02})
	_, err := ReadFloat(r, 4)
	if err == nil {
		t.Fatal("expected read error for float32")
	}

	// 8-byte float but only 4 bytes
	r = bytes.NewReader([]byte{0x01, 0x02, 0x03, 0x04})
	_, err = ReadFloat(r, 8)
	if err == nil {
		t.Fatal("expected read error for float64")
	}
}

func TestReadString_ExceedsMaxSize(t *testing.T) {
	r := strings.NewReader("x")
	_, err := ReadString(r, MaxElementSize+1)
	if err == nil {
		t.Fatal("expected size limit error")
	}
}

func TestReadString_NegativeSize(t *testing.T) {
	r := strings.NewReader("x")
	_, err := ReadString(r, -1)
	if err == nil {
		t.Fatal("expected error for negative size")
	}
}

func TestReadString_ZeroSize(t *testing.T) {
	r := strings.NewReader("x")
	got, err := ReadString(r, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestReadString_TrailingNulls(t *testing.T) {
	r := bytes.NewReader([]byte{'h', 'i', 0, 0})
	got, err := ReadString(r, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hi" {
		t.Errorf("expected %q, got %q", "hi", got)
	}
}

func TestReadBytes_ExceedsMaxSize(t *testing.T) {
	r := strings.NewReader("x")
	_, err := ReadBytes(r, MaxElementSize+1)
	if err == nil {
		t.Fatal("expected size limit error")
	}
}

// TestReadBytes_HugeDeclaredSizeNoAlloc guards against a memory DoS: a tiny
// input that declares a near-MaxElementSize element must fail with the data
// available, NOT allocate the declared size upfront. (If it over-allocated, this
// test would spike ~256 MB / be killed under the race detector's memory limits.)
func TestReadBytes_HugeDeclaredSizeNoAlloc(t *testing.T) {
	r := bytes.NewReader([]byte{1, 2, 3, 4, 5})
	_, err := ReadBytes(r, 256*1024*1024) // 256 MB declared, 5 bytes available
	if err == nil {
		t.Fatal("expected error for truncated huge element")
	}
}

func TestCheckSizeBoundary(t *testing.T) {
	if err := checkSize(MaxElementSize); err != nil {
		t.Errorf("checkSize(MaxElementSize) = %v, want nil (cap is inclusive)", err)
	}
	if err := checkSize(MaxElementSize + 1); err == nil {
		t.Error("checkSize(MaxElementSize+1) = nil, want error")
	}
	if err := checkSize(-1); err == nil {
		t.Error("checkSize(-1) = nil, want error")
	}
}

func TestReadBytes_NegativeSize(t *testing.T) {
	r := strings.NewReader("x")
	_, err := ReadBytes(r, -1)
	if err == nil {
		t.Fatal("expected error for negative size")
	}
}

func TestReadBytes_ZeroSize(t *testing.T) {
	r := strings.NewReader("x")
	got, err := ReadBytes(r, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got len %d", len(got))
	}
}

func TestReadString_ReadError(t *testing.T) {
	r := bytes.NewReader([]byte{0x01})
	_, err := ReadString(r, 5)
	if err == nil {
		t.Fatal("expected read error")
	}
}

func TestReadBytes_ReadError(t *testing.T) {
	r := bytes.NewReader([]byte{0x01})
	_, err := ReadBytes(r, 5)
	if err == nil {
		t.Fatal("expected read error")
	}
}
