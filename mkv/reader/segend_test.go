package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestUnknownElementEndingTheSegmentIsNoJunk: an element mkvgo does not know that ends exactly where the Segment does
// is skipped by its size, by Read as by ReadMeta, not taken for junk: a sound file reports no resynced byte.
func TestUnknownElementEndingTheSegmentIsNoJunk(t *testing.T) {
	info := wrapEl(mkv.IDInfo, uintEl(mkv.IDTimecodeScale, 1000000))
	unknown := wrapEl(0x4FFF, []byte{1, 2, 3, 4})
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&b, mkv.IDSegment, int64(len(info)+len(unknown)))
	b.Write(info)
	b.Write(unknown)
	for name, read := range map[string]func() (*mkv.Container, error){
		"Read": func() (*mkv.Container, error) { return Read(context.Background(), bytes.NewReader(b.Bytes()), "x.mkv") },
		"ReadMeta": func() (*mkv.Container, error) {
			return ReadMeta(context.Background(), bytes.NewReader(b.Bytes()), "x.mkv")
		},
	} {
		c, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.ResyncedBytes != 0 {
			t.Errorf("%s: ResyncedBytes %d on a sound file", name, c.ResyncedBytes)
		}
	}
}
