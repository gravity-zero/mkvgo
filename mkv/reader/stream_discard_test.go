package reader

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// onlyReader hides every method but Read: a pipe, a socket, a decompressor.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }

// TestStreamReaderSkipsALargeFilteredBlock: on a source that cannot seek, a block past seekSkipMin of a track the
// walk filters out (or a payload a header-only walk skips) is read through, never seeked over.
func TestStreamReaderSkipsALargeFilteredBlock(t *testing.T) {
	block := func(track byte, n int) []byte {
		var sb bytes.Buffer
		ebml.WriteDataSize(&sb, int64(track))
		sb.Write([]byte{0x00, 0x00, 0x80})
		sb.Write(bytes.Repeat([]byte{track}, n))
		var b bytes.Buffer
		ebml.WriteElementHeader(&b, mkv.IDSimpleBlock, int64(sb.Len()))
		b.Write(sb.Bytes())
		return b.Bytes()
	}
	var body bytes.Buffer
	ebml.WriteElementHeader(&body, mkv.IDTimestamp, 1)
	body.WriteByte(0)
	body.Write(block(1, 2<<20))
	body.Write(block(2, 10))
	body.Write(block(1, 2<<20))
	var seg bytes.Buffer
	seg.Write(wrapEl(mkv.IDCluster, body.Bytes()))
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	for name, set := range map[string]func(*BlockReader){
		"filtered":    func(br *BlockReader) { br.KeepTracks(2) },
		"header only": func(br *BlockReader) { br.SetHeaderOnly(true) },
	} {
		br, err := NewStreamBlockReader(onlyReader{bytes.NewReader(buf.Bytes())}, 1_000_000)
		if err != nil {
			t.Fatal(name, err)
		}
		set(br)
		n := 0
		for {
			_, err := br.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%s: after %d blocks: %v", name, n, err)
			}
			n++
		}
	}
}
