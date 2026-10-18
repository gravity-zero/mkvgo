package reader

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// overrunClusters is two Clusters: the first holds a sound block 0xAA and a SimpleBlock declaring the whole second
// Cluster as its own, the second a block 0xBB at 1000 ms. With blockGroup, the overrunning block sits in a BlockGroup.
func overrunClusters(blockGroup bool) (data []byte, second int64) {
	block := func(payload byte) []byte { return []byte{0x81, 0x00, 0x00, 0x80, payload, payload} }
	var c2 bytes.Buffer
	c2.Write(uintEl(mkv.IDTimestamp, 1000))
	c2.Write(wrapEl(mkv.IDSimpleBlock, block(0xBB)))
	cluster2 := wrapEl(mkv.IDCluster, c2.Bytes())

	var c1 bytes.Buffer
	c1.Write(uintEl(mkv.IDTimestamp, 0))
	c1.Write(wrapEl(mkv.IDSimpleBlock, block(0xAA)))
	overrun := len(block(0xCC)) + len(cluster2) // the second Cluster read as this block's data
	if blockGroup {
		var g bytes.Buffer
		ebml.WriteElementHeader(&g, mkv.IDBlock, int64(overrun))
		g.Write(block(0xCC))
		ebml.WriteElementHeader(&c1, mkv.IDBlockGroup, int64(g.Len()))
		c1.Write(g.Bytes())
	} else {
		ebml.WriteElementHeader(&c1, mkv.IDSimpleBlock, int64(overrun))
		c1.Write(block(0xCC))
	}
	var seg bytes.Buffer
	seg.Write(wrapEl(mkv.IDCluster, c1.Bytes()))
	second = int64(seg.Len())
	seg.Write(cluster2)
	var buf bytes.Buffer
	ebml.WriteElementHeader(&buf, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&buf, mkv.IDSegment, int64(seg.Len()))
	return append(buf.Bytes(), seg.Bytes()...), second + int64(buf.Len())
}

// TestBlockPastItsClusterIsDamage: a block declaring more bytes than its Cluster holds swallowed the next Cluster
// and lost its blocks without a word. It is damage: a strict walk refuses it, resuming on the next Cluster, and a
// tolerant walk skips to that Cluster, records the range and reads its block.
func TestBlockPastItsClusterIsDamage(t *testing.T) {
	for _, group := range []bool{false, true} {
		data, second := overrunClusters(group)

		br, err := NewBlockReader(bytes.NewReader(data), 1000000)
		if err != nil {
			t.Fatal(err)
		}
		if b, err := br.Next(); err != nil || b.Data[0] != 0xAA {
			t.Fatalf("group %v: first block %v, %v", group, b.Data, err)
		}
		_, err = br.Next()
		var de *DamageError
		if !errors.As(err, &de) || de.Resume != second {
			t.Errorf("group %v: strict walk past the overrunning block: %v, want a DamageError resuming at %d", group, err, second)
		}

		br, err = NewBlockReader(bytes.NewReader(data), 1000000)
		if err != nil {
			t.Fatal(err)
		}
		br.SetSkipDamage(true)
		var got []byte
		for {
			b, err := br.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("group %v: tolerant walk: %v", group, err)
			}
			got = append(got, b.Data[0])
		}
		if !bytes.Equal(got, []byte{0xAA, 0xBB}) || len(br.SkippedRanges()) != 1 {
			t.Errorf("group %v: tolerant walk read %x with %d ranges skipped, want aa bb and 1", group, got, len(br.SkippedRanges()))
		}
	}
}
