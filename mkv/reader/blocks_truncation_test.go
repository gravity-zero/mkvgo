package reader

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestClusterCutBetweenTwoElementsIsTruncation: a file ending between two children of a Cluster of known size was
// read as a clean end, its missing blocks lost without a word.
func TestClusterCutBetweenTwoElementsIsTruncation(t *testing.T) {
	block := wrapEl(mkv.IDSimpleBlock, []byte{0x81, 0x00, 0x00, 0x80, 0xAA})
	var c bytes.Buffer
	c.Write(uintEl(mkv.IDTimestamp, 0))
	c.Write(block)
	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(c.Len()+len(block)))
	seg.Write(c.Bytes())
	var buf bytes.Buffer
	ebml.WriteElementHeader(&buf, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&buf, mkv.IDSegment, -1)
	data := append(buf.Bytes(), seg.Bytes()...)

	for _, skip := range []bool{false, true} {
		br, err := NewBlockReader(bytes.NewReader(data), 1000000)
		if err != nil {
			t.Fatal(err)
		}
		br.SetSkipDamage(skip)
		if b, err := br.Next(); err != nil || b.Data[0] != 0xAA {
			t.Fatalf("skip %v: first block %v, %v", skip, b.Data, err)
		}
		_, err = br.Next()
		if !skip && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("strict walk past the cut: %v, want io.ErrUnexpectedEOF", err)
		}
		if skip && (!errors.Is(err, io.EOF) || len(br.SkippedRanges()) != 1) {
			t.Errorf("tolerant walk past the cut: %v, skipped %v, want io.EOF with the cut recorded", err, br.SkippedRanges())
		}
	}
}
