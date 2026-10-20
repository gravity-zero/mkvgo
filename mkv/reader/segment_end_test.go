package reader

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestSegmentEndBoundsAWalkOpenedAtAnOffset: the readers state where the Segment ends, and a tolerant walk opened at
// a Cluster ends there with SetSegmentEnd, the junk after it trailing data, not damage passed over.
func TestSegmentEndBoundsAWalkOpenedAtAnOffset(t *testing.T) {
	info := wrapEl(mkv.IDInfo, uintEl(mkv.IDTimecodeScale, 1000000))
	cluster := wrapEl(mkv.IDCluster, append(uintEl(mkv.IDTimestamp, 0), wrapEl(mkv.IDSimpleBlock, []byte{0x81, 0, 0, 0x80, 1, 2, 3})...))
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&b, mkv.IDSegment, int64(len(info)+len(cluster)))
	b.Write(info)
	clusterAt := int64(b.Len())
	b.Write(cluster)
	segEnd := int64(b.Len())
	b.Write(bytes.Repeat([]byte{0x55}, 4096)) // junk after the Segment's declared end
	data := b.Bytes()
	for name, read := range map[string]func() (*mkv.Container, error){
		"Read":     func() (*mkv.Container, error) { return Read(context.Background(), bytes.NewReader(data), "x.mkv") },
		"ReadMeta": func() (*mkv.Container, error) { return ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv") },
	} {
		c, err := read()
		if err != nil {
			t.Fatal(err)
		}
		if c.SegmentEnd != segEnd {
			t.Errorf("%s: SegmentEnd %d, want %d", name, c.SegmentEnd, segEnd)
		}
	}
	walk := func(bound bool) (blocks int, skipped int64) {
		br, err := NewBlockReaderAt(bytes.NewReader(data), 1000000, clusterAt)
		if err != nil {
			t.Fatal(err)
		}
		br.SetSkipDamage(true)
		if bound {
			br.SetSegmentEnd(segEnd)
		}
		for {
			if _, err := br.Next(); err == io.EOF {
				return blocks, br.SkippedBytes()
			} else if err != nil {
				t.Fatal(err)
			}
			blocks++
		}
	}
	if blocks, skipped := walk(true); blocks != 1 || skipped != 0 {
		t.Errorf("bounded walk: %d blocks, %d bytes passed over as damage; want 1 and 0", blocks, skipped)
	}
	if _, skipped := walk(false); skipped == 0 {
		t.Error("an unbounded walk passes the junk over as damage: the test no longer tells the two apart")
	}
}
