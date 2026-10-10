package reader

import (
	"bytes"
	"io"
	"testing"
)

// DataOffset points at each frame's payload in the input, unlaced and laced, in header-only mode too.
func TestBlockDataOffset(t *testing.T) {
	cases := map[string][]byte{
		"unlaced": {0x81, 0x00, 0x00, 0x80, 0xAA, 0xBB, 0xCC},
		"laced":   {0x81, 0x00, 0x00, 0x84, 0x01, 0xAA, 0xBB}, // fixed lacing, 2 frames of 1 byte
	}
	for name, payload := range cases {
		input, _ := io.ReadAll(buildBlockReaderInput(clusterWithSimpleBlock(payload)))
		for _, headerOnly := range []bool{false, true} {
			br, err := NewBlockReader(bytes.NewReader(input), 1000000)
			if err != nil {
				t.Fatal(err)
			}
			br.SetHeaderOnly(headerOnly)
			for i := 0; ; i++ {
				b, err := br.Next()
				if err != nil {
					break
				}
				if b.DataOffset <= 0 || b.DataOffset+b.Size > int64(len(input)) {
					t.Fatalf("%s frame %d headerOnly=%v: offset %d size %d out of input (%d bytes)", name, i, headerOnly, b.DataOffset, b.Size, len(input))
				}
				at := input[b.DataOffset : b.DataOffset+b.Size]
				if !headerOnly && !bytes.Equal(at, b.Data) {
					t.Errorf("%s frame %d: input at DataOffset % x, Data % x", name, i, at, b.Data)
				}
				if headerOnly && b.Data != nil {
					t.Errorf("%s frame %d headerOnly: Data must stay nil", name, i)
				}
				if want := []byte{0xAA, 0xBB}[i]; name == "laced" && at[0] != want {
					t.Errorf("%s frame %d: payload %x, want %x", name, i, at[0], want)
				}
			}
		}
	}
}
