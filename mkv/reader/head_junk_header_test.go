package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/internal/livefixture"
	"github.com/gravity-zero/mkvgo/mkv"
)

// beforeFirstCluster returns the fixture with extra spliced in ahead of its first Cluster, a sized Segment grown by
// as much.
func beforeFirstCluster(t *testing.T, o livefixture.Options, extra []byte) []byte {
	t.Helper()
	data := livefixture.Build(o)
	r := bytes.NewReader(data)
	h, n, err := ebml.ReadElementHeader(r)
	if err != nil {
		t.Fatal(err)
	}
	segAt := int64(n) + h.Size
	if _, err := r.Seek(segAt, 0); err != nil {
		t.Fatal(err)
	}
	seg, segHdr, err := ebml.ReadElementHeader(r)
	if err != nil || seg.ID != mkv.IDSegment {
		t.Fatalf("Segment header: 0x%X, %v", seg.ID, err)
	}
	cluster := bytes.Index(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	if cluster < 0 {
		t.Fatal("no Cluster in the fixture")
	}
	out := append([]byte(nil), data[:cluster]...)
	out = append(out, extra...)
	out = append(out, data[cluster:]...)
	if seg.Size >= 0 {
		var size bytes.Buffer
		if _, err := ebml.WriteDataSizeWidth(&size, seg.Size+int64(len(extra)), segHdr-4); err != nil {
			t.Fatal(err)
		}
		copy(out[segAt+4:], size.Bytes())
	}
	return out
}

// TestHeadJunkThatReadsAsAnElement: bytes left ahead of the first Cluster that happen to decode as an element header
// (the tail of an old tag string, "0000" then two null bytes: an unknown ID declaring 1 MiB) are junk to every reader,
// the block walk included. It used to skip them by the size they declare: past the end of the file, reported as a
// source cut short, or into the media, losing the Clusters it landed past without a word.
func TestHeadJunkThatReadsAsAnElement(t *testing.T) {
	ctx := context.Background()
	shapes := []struct {
		name string
		opts livefixture.Options
	}{
		{"live recording", livefixture.Options{}},
		{"finished file", livefixture.Options{SizedClusters: true, PayloadBytes: 64}},
	}
	junks := []struct {
		name string
		junk []byte
	}{
		{"declares more than the file holds", []byte("0000\x00\x00")},
		{"declares a few bytes into the Cluster", []byte{'0', '0', '0', 0x88, 0x00, 0x00}},
		{"declares a size that ends inside a later Cluster", []byte{'0', '0', '0', 0x40, 0x60, 0x00}},
	}
	for _, s := range shapes {
		for _, j := range junks {
			t.Run(s.name+", "+j.name, func(t *testing.T) {
				data := beforeFirstCluster(t, s.opts, j.junk)
				want := int64(len(j.junk))

				br, err := NewBlockReader(bytes.NewReader(data), 1_000_000)
				if err != nil {
					t.Fatalf("NewBlockReader: %v", err)
				}
				if n, first, last := drainBlocks(t, br); n != livefixture.Blocks || first != livefixture.FirstTimestampMs || last != livefixture.LastBlockMs {
					t.Errorf("blocks = %d [%d..%d], want %d [%d..%d]", n, first, last,
						livefixture.Blocks, livefixture.FirstTimestampMs, livefixture.LastBlockMs)
				}

				tolerant, err := NewBlockReader(bytes.NewReader(data), 1_000_000)
				if err != nil {
					t.Fatalf("NewBlockReader: %v", err)
				}
				tolerant.SetSkipDamage(true)
				if n, _, _ := drainBlocks(t, tolerant); n != livefixture.Blocks || tolerant.SkippedBytes() != want {
					t.Errorf("tolerant walk: %d blocks, %d bytes skipped; want %d blocks and the %d junk bytes",
						n, tolerant.SkippedBytes(), livefixture.Blocks, want)
				}

				full, err := Read(ctx, bytes.NewReader(data), "junk.mkv")
				if err != nil {
					t.Fatalf("Read: %v", err)
				}
				if full.ResyncedBytes != want {
					t.Errorf("Read: ResyncedBytes %d, want %d", full.ResyncedBytes, want)
				}
			})
		}
	}
}

// TestHeadUnknownElementIsNotJunk: an element of an ID Matroska does not name, whole and followed by the first
// Cluster, is skipped by its size as before: nothing is reported as passed over.
func TestHeadUnknownElementIsNotJunk(t *testing.T) {
	for _, opts := range []livefixture.Options{{}, {SizedClusters: true}} {
		data := beforeFirstCluster(t, opts, []byte{'0', '0', '0', 0x84, 1, 2, 3, 4})
		br, err := NewBlockReader(bytes.NewReader(data), 1_000_000)
		if err != nil {
			t.Fatalf("NewBlockReader: %v", err)
		}
		br.SetSkipDamage(true)
		if n, _, _ := drainBlocks(t, br); n != livefixture.Blocks || br.SkippedBytes() != 0 {
			t.Errorf("sized %v: %d blocks, %d bytes skipped; want %d blocks and none skipped", opts.SizedClusters, n, br.SkippedBytes(), livefixture.Blocks)
		}
		stream, err := NewStreamBlockReader(bytes.NewReader(data), 1_000_000)
		if err != nil {
			t.Fatalf("NewStreamBlockReader: %v", err)
		}
		if n, _, _ := drainBlocks(t, stream); n != livefixture.Blocks {
			t.Errorf("sized %v: forward-only walk: %d blocks, want %d", opts.SizedClusters, n, livefixture.Blocks)
		}
		c, err := Read(context.Background(), bytes.NewReader(data), "unknown.mkv")
		if err != nil || c.ResyncedBytes != 0 {
			t.Errorf("sized %v: Read: ResyncedBytes %d, err %v; want a sound file", opts.SizedClusters, c.ResyncedBytes, err)
		}
	}
}
