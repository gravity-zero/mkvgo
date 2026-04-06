package ebml

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

func TestElementIDLen(t *testing.T) {
	for _, tt := range []struct {
		id   uint32
		want int
	}{
		{0x86, 1}, {0xFF, 1},
		{0x4286, 2}, {0x42F7, 2},
		{0x2B6661, 3},
		{0x1A45DFA3, 4},
	} {
		if got := ElementIDLen(tt.id); got != tt.want {
			t.Errorf("ElementIDLen(0x%X) = %d, want %d", tt.id, got, tt.want)
		}
	}
}

func TestDataSizeLen(t *testing.T) {
	for _, tt := range []struct {
		size int64
		want int
	}{
		{0, 1}, {126, 1},
		{127, 2}, {16382, 2},
		{16383, 3}, {2097150, 3},
		{2097151, 4}, {268435454, 4},
		{268435455, 5},
	} {
		if got := DataSizeLen(tt.size); got != tt.want {
			t.Errorf("DataSizeLen(%d) = %d, want %d", tt.size, got, tt.want)
		}
	}
}

func TestUintLen(t *testing.T) {
	for _, tt := range []struct {
		val  uint64
		want int
	}{
		{0, 1}, {1, 1}, {255, 1},
		{256, 2}, {65535, 2},
		{65536, 3},
		{math.MaxUint64, 8},
	} {
		if got := UintLen(tt.val); got != tt.want {
			t.Errorf("UintLen(%d) = %d, want %d", tt.val, got, tt.want)
		}
	}
}

func roundTripElementID(t *testing.T, id uint32) {
	t.Helper()
	var buf bytes.Buffer
	n, err := WriteElementID(&buf, id)
	assertNoErr(t, err)
	assertEqual(t, n, ElementIDLen(id), "bytes written")

	got, n2, err := ReadElementID(&buf)
	assertNoErr(t, err)
	assertEqual(t, got, id, "element ID")
	assertEqual(t, n2, n, "bytes consumed")
}

func TestWriteReadElementID(t *testing.T) {
	for _, id := range []uint32{0x86, 0xA3, 0x4286, 0x42F7, 0x2B6661, 0x1A45DFA3} {
		roundTripElementID(t, id)
	}
}

func TestWriteReadDataSize(t *testing.T) {
	for _, size := range []int64{0, 1, 126, 127, 16382, 16383, 2097150, 2097151, 268435454} {
		var buf bytes.Buffer
		n, err := WriteDataSize(&buf, size)
		assertNoErr(t, err)

		got, n2, err := ReadDataSize(&buf)
		assertNoErr(t, err)
		assertEqual(t, got, size, "data size")
		assertEqual(t, n2, n, "bytes consumed")
	}
}

func TestWriteReadDataSizeUnknown(t *testing.T) {
	var buf bytes.Buffer
	n, err := WriteDataSize(&buf, -1)
	assertNoErr(t, err)
	assertEqual(t, n, 8, "bytes written")

	got, _, err := ReadDataSize(&buf)
	assertNoErr(t, err)
	assertEqual(t, got, int64(-1), "unknown size")
}

func TestWriteReadElementHeader(t *testing.T) {
	for _, tt := range []struct {
		id   uint32
		size int64
	}{
		{IDEBMLVersion, 1}, {IDEBMLHeader, 31}, {IDDocType, 8},
		{0x1A45DFA3, 2097151}, {0xA3, 0},
	} {
		var buf bytes.Buffer
		n, err := WriteElementHeader(&buf, tt.id, tt.size)
		assertNoErr(t, err)
		assertEqual(t, n, ElementHeaderLen(tt.id, tt.size), "bytes written")

		hdr, n2, err := ReadElementHeader(&buf)
		assertNoErr(t, err)
		assertEqual(t, hdr.ID, tt.id, "ID")
		assertEqual(t, hdr.Size, tt.size, "Size")
		assertEqual(t, n2, n, "bytes consumed")
	}
}

func TestWriteReadUint(t *testing.T) {
	for _, tt := range []struct {
		val uint64
		n   int
	}{
		{0, 1}, {1, 1}, {255, 1},
		{256, 2}, {1000000, 3},
		{math.MaxUint32, 4}, {math.MaxUint64, 8},
	} {
		var buf bytes.Buffer
		_, err := WriteUint(&buf, tt.val, tt.n)
		assertNoErr(t, err)

		got, err := ReadUint(&buf, int64(tt.n))
		assertNoErr(t, err)
		assertEqual(t, got, tt.val, "uint value")
	}
}

func TestWriteReadFloat(t *testing.T) {
	for _, val := range []float64{0, 1, -1, 3.14159, math.MaxFloat64, math.SmallestNonzeroFloat64} {
		var buf bytes.Buffer
		_, err := WriteFloat(&buf, val)
		assertNoErr(t, err)

		got, err := ReadFloat(&buf, 8)
		assertNoErr(t, err)
		assertEqual(t, got, val, "float64 value")
	}
}

func TestWriteReadFloat32(t *testing.T) {
	for _, val := range []float32{0, 1, -1, 3.14159} {
		var buf bytes.Buffer
		_, err := WriteFloat32(&buf, val)
		assertNoErr(t, err)

		got, err := ReadFloat(&buf, 4)
		assertNoErr(t, err)
		assertEqual(t, float32(got), val, "float32 value")
	}
}

func TestWriteReadString(t *testing.T) {
	for _, val := range []string{"", "matroska", "webm", "hello world"} {
		var buf bytes.Buffer
		_, err := WriteString(&buf, val)
		assertNoErr(t, err)

		got, err := ReadString(&buf, int64(len(val)))
		assertNoErr(t, err)
		assertEqual(t, got, val, "string value")
	}
}

func TestWriteReadBytes(t *testing.T) {
	for _, val := range [][]byte{{}, {0x00}, {0xDE, 0xAD, 0xBE, 0xEF}} {
		var buf bytes.Buffer
		_, err := WriteBytes(&buf, val)
		assertNoErr(t, err)

		got, err := ReadBytes(&buf, int64(len(val)))
		assertNoErr(t, err)
		if !bytes.Equal(got, val) {
			t.Errorf("round-trip bytes: wrote %x, read %x", val, got)
		}
	}
}

func TestDataSizeLen_Negative(t *testing.T) {
	// Negative size loops through all widths without matching => returns 8
	got := DataSizeLen(-1)
	if got != 8 {
		t.Errorf("DataSizeLen(-1) = %d, want 8", got)
	}
}

func TestElementHeaderLen_NegativeSize(t *testing.T) {
	// Negative size => unknown => ID len + 8
	got := ElementHeaderLen(0x86, -1)
	want := ElementIDLen(0x86) + 8
	if got != want {
		t.Errorf("ElementHeaderLen(0x86, -1) = %d, want %d", got, want)
	}
}

func TestWriteDataSize_Negative(t *testing.T) {
	var buf bytes.Buffer
	n, err := WriteDataSize(&buf, -1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 8 {
		t.Errorf("WriteDataSize(-1) wrote %d bytes, want 8", n)
	}
	want := []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("WriteDataSize(-1) = %x, want %x", buf.Bytes(), want)
	}
}

func TestWriteElementHeader_Error(t *testing.T) {
	// Writer that always fails
	w := &errWriter{}
	_, err := WriteElementHeader(w, 0x86, 10)
	if err == nil {
		t.Fatal("expected write error")
	}
}

type errWriter struct{}

func (e *errWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

// --- helpers ---

func assertNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertEqual[T comparable](t *testing.T, got, want T, label string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}
