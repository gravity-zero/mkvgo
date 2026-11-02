package reader

import (
	"bytes"
	"io"
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
