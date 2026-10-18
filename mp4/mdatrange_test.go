package mp4

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestMdatRanges: the payloads of the top-level mdat boxes, a 64-bit size and a to-the-end size included, are where
// a sample table's chunks may lie; a chunk at offset 0 lies in the ftyp, outside every one.
func TestMdatRanges(t *testing.T) {
	box := func(typ string, payload int) []byte {
		b := make([]byte, 8+payload)
		binary.BigEndian.PutUint32(b, uint32(8+payload))
		copy(b[4:], typ)
		return b
	}
	var f bytes.Buffer
	f.Write(box("ftyp", 12))     // 0-20
	f.Write(box("mdat", 100))    // payload 28-128
	large := make([]byte, 16+50) // payload 144-194
	binary.BigEndian.PutUint32(large, 1)
	copy(large[4:], "mdat")
	binary.BigEndian.PutUint64(large[8:], uint64(len(large)))
	f.Write(large)
	f.Write(box("moov", 10))
	last := box("mdat", 30) // size 0: to the end, payload 220-250
	binary.BigEndian.PutUint32(last, 0)
	f.Write(last)
	ranges, err := mdatRanges(bytes.NewReader(f.Bytes()), int64(f.Len()))
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]int64{{28, 128}, {144, 194}, {220, 250}}
	if len(ranges) != len(want) {
		t.Fatalf("ranges %v, want %v", ranges, want)
	}
	for i := range want {
		if ranges[i] != want[i] {
			t.Errorf("range %d = %v, want %v", i, ranges[i], want[i])
		}
	}
	if inMdat(ranges, 0, 8) || !inMdat(ranges, 28, 100) || inMdat(ranges, 120, 20) || !inMdat(ranges, 220, 30) {
		t.Error("inMdat: offset 0 must lie outside, a whole payload inside, a run across a box end outside")
	}
	if _, err := mdatRanges(bytes.NewReader(box("mdat", 4)[:10]), 10); err == nil {
		t.Error("a box declaring more bytes than the file holds was accepted")
	}
}
