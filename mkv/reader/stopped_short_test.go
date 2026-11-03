package reader

import (
	"bytes"
	"io"
	"math/bits"
	"testing"
)

// TestBlockReaderStoppedShort: a walk that ends because the source stops
// inside an element says so after the fact, so a caller that takes that end
// for the end of the media can still report the cut; a whole file does not.
func TestBlockReaderStoppedShort(t *testing.T) {
	avcc := scanAvcC([][]byte{scanSPS{}.nal()}, [][]byte{scanPPS(0, 0, false)})
	whole := interleavedMKV(avcc, 0, 20, au(scanSlice{lsb: 2}.nal()))
	for name, c := range map[string]struct {
		data []byte
		want bool
	}{
		"whole":        {whole, false},
		"cut in block": {whole[:len(whole)-3], true},
	} {
		br, err := NewBlockReader(bytes.NewReader(c.data), 1_000_000)
		if err != nil {
			t.Fatal(err)
		}
		for err == nil {
			_, err = br.Next()
		}
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			t.Fatalf("%s: walk ended on %v", name, err)
		}
		if got := br.StoppedShort(); got != c.want {
			t.Errorf("%s: StoppedShort = %v, want %v (walk ended on %v)", name, got, c.want, err)
		}
	}
}

// streamedSegment is data with its Segment size rewritten as unknown (a streamed write).
func streamedSegment(t *testing.T, data []byte) []byte {
	t.Helper()
	out := append([]byte{}, data...)
	seg := bytes.Index(out, []byte{0x18, 0x53, 0x80, 0x67})
	if seg < 0 || out[seg+4] == 0 {
		t.Fatal("fixture has no Segment")
	}
	n := bits.LeadingZeros8(out[seg+4]) + 1 // the size's width: all its value bits set is "unknown"
	out[seg+4] |= 0xFF >> n
	for i := 1; i < n; i++ {
		out[seg+4+i] = 0xFF
	}
	return out
}

// id3Tail is an ID3v2 tag header, as a tagger appends it to a file: read as EBML, an element running past the end.
var id3Tail = []byte{'I', 'D', '3', 0x04, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 'T', 'I', 'T', '2'}

// oversizedVoid is a Void element declaring 4096 bytes, with 6 present.
var oversizedVoid = []byte{0xEC, 0x50, 0x00, 0, 0, 0, 0, 0, 0}

// TestBlockReaderStreamedTrailingBytes: a Segment of unknown size followed by bytes that are not a Matroska element
// (an ID3 tag) or by a Void running past the end is whole: the walk ends there cleanly, not StoppedShort. Cut inside
// a block, it still is.
func TestBlockReaderStreamedTrailingBytes(t *testing.T) {
	avcc := scanAvcC([][]byte{scanSPS{}.nal()}, [][]byte{scanPPS(0, 0, false)})
	whole := streamedSegment(t, interleavedMKV(avcc, 0, 20, au(scanSlice{lsb: 2}.nal())))
	for name, c := range map[string]struct {
		data []byte
		want bool
	}{
		"ID3 tail":       {append(append([]byte{}, whole...), id3Tail...), false},
		"oversized Void": {append(append([]byte{}, whole...), oversizedVoid...), false},
		"cut in block":   {whole[:len(whole)-3], true},
	} {
		for _, skip := range []bool{false, true} {
			br, err := NewBlockReader(bytes.NewReader(c.data), 1_000_000)
			if err != nil {
				t.Fatal(err)
			}
			br.SetSkipDamage(skip)
			n := 0
			for err == nil {
				if _, err = br.Next(); err == nil {
					n++
				}
			}
			if want := 21 - b2i(c.want); n != want {
				t.Errorf("%s (skip %v): %d blocks, want %d", name, skip, n, want)
			}
			if !c.want && err != io.EOF {
				t.Errorf("%s (skip %v): walk ended on %v, want io.EOF", name, skip, err)
			}
			if got := br.StoppedShort(); got != c.want {
				t.Errorf("%s (skip %v): StoppedShort = %v, want %v (walk ended on %v)", name, skip, got, c.want, err)
			}
		}
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
