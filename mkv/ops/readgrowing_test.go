package ops

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"testing"
)

// A body read in place of its declared size allocates in proportion to the bytes that arrive.
func TestReadBodyAllocatesWhatArrives(t *testing.T) {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err := readBody(bytes.NewReader([]byte("ten bytes!")), nil, 200<<20)
	runtime.ReadMemStats(&after)
	if err != io.ErrUnexpectedEOF || string(got) != "ten bytes!" {
		t.Fatalf("readBody = %q, %v; want the ten bytes and io.ErrUnexpectedEOF", got, err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 16<<20 {
		t.Errorf("%d bytes allocated for ten", n)
	}
	whole := bytes.Repeat([]byte{7}, 20<<20)
	if got, err := readBody(bytes.NewReader(whole), make([]byte, 0, 1<<10), int64(len(whole))); err != nil || !bytes.Equal(got, whole) {
		t.Fatalf("a body past one step: %d bytes, %v", len(got), err)
	}
}

// retime's scan allocates no cluster size a few bytes declare.
func TestRetimeScanAllocatesNoDeclaredClusterSize(t *testing.T) {
	data := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x80, // EBML header, empty
		0x18, 0x53, 0x80, 0x67, 0x01, 0x00, 0x00, 0x00, 0x0C, 0x80, 0x00, 0x20, // Segment declaring 200 MiB + 32
		0x1F, 0x43, 0xB6, 0x75, 0x01, 0x00, 0x00, 0x00, 0x0C, 0x80, 0x00, 0x00, // Cluster declaring 200 MiB
		0xE7, 0x81, 0x00} // its Timestamp, then the file ends
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, _, err := retimeScan(context.Background(), bytes.NewReader(data), int64(len(data)), map[uint64]int64{1: 10}, nil, 1<<20); err == nil {
		t.Fatal("a cluster cut short must not scan")
	}
	runtime.ReadMemStats(&after)
	if n := after.TotalAlloc - before.TotalAlloc; n > 16<<20 {
		t.Errorf("%d bytes allocated for a %d-byte file", n, len(data))
	}
}
