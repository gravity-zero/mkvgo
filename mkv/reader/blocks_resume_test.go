package reader

import (
	"bytes"
	"io"
	"reflect"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// buildMixedBlockFixture writes clusters holding the three shapes a real source
// mixes: plain SimpleBlocks, a BlockGroup (Block + BlockDuration), and a
// fixed-laced SimpleBlock whose frames share one stored timecode.
func buildMixedBlockFixture(t *testing.T, numClusters int) []byte {
	t.Helper()
	var seg bytes.Buffer
	for c := 0; c < numClusters; c++ {
		var cluster bytes.Buffer
		ts := uint64(c * 1000)
		ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, int64(ebml.UintLen(ts)))
		ebml.WriteUint(&cluster, ts, ebml.UintLen(ts))

		writeSimpleBlock(&cluster, 1, 0, 0x80, append(make([]byte, 3<<10), byte(c)))

		// A laced audio block: 3 frames of 2 bytes on one timecode.
		lacedPayload := []byte{
			0x82,       // track 2
			0x00, 0x0A, // relative timecode 10
			0x04,                         // flags: fixed lacing
			0x02,                         // lace count byte: 3 frames
			'a', 'b', 'c', 'd', 'e', 'f', // 3 x 2 bytes
		}
		ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(lacedPayload)))
		cluster.Write(lacedPayload)

		// A BlockGroup: Block + BlockDuration.
		var group bytes.Buffer
		blockPayload := append([]byte{0x81, 0x00, 0x14, 0x00}, make([]byte, 2<<10)...)
		ebml.WriteElementHeader(&group, mkv.IDBlock, int64(len(blockPayload)))
		group.Write(blockPayload)
		ebml.WriteElementHeader(&group, mkv.IDBlockDuration, 1)
		ebml.WriteUint(&group, 40, 1)
		ebml.WriteElementHeader(&cluster, mkv.IDBlockGroup, int64(group.Len()))
		cluster.Write(group.Bytes())

		writeSimpleBlock(&cluster, 1, 30, 0x00, append(make([]byte, 4<<10), byte(c)))

		ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(cluster.Len()))
		seg.Write(cluster.Bytes())
	}
	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&full, mkv.IDSegment, int64(seg.Len()))
	full.Write(seg.Bytes())
	return full.Bytes()
}

// A walk resumed at any block's Pos must deliver exactly the tail of the full
// walk from that block on - that is what lets a segment start on its own first
// block instead of on its cluster's header, and re-read nothing ahead of it.
// Every block shape is exercised: SimpleBlock, BlockGroup (its Pos must name
// the GROUP, so the duration is parsed again), and a laced block (whose frames
// share the enclosing block's Pos, so resuming there replays the whole lace).
func TestBlockReaderResumeAtAnyBlock(t *testing.T) {
	fixture := buildMixedBlockFixture(t, 4)

	full, err := NewBlockReader(bytes.NewReader(fixture), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	var want []mkv.Block
	var at []BlockPos
	for {
		b, err := full.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("full walk: %v", err)
		}
		want = append(want, b)
		at = append(at, full.Pos())
	}
	if len(want) < 12 {
		t.Fatalf("fixture delivered %d blocks, too few to test resumption", len(want))
	}

	for i := range want {
		if !at[i].Valid() {
			t.Fatalf("block %d: Pos is not a valid resume point", i)
		}
		br, err := NewBlockReaderFrom(bytes.NewReader(fixture), 1_000_000, at[i])
		if err != nil {
			t.Fatalf("resume at block %d: %v", i, err)
		}
		got := drain(t, br)

		// The frames of a lace share their block's Pos, so resuming on one
		// replays the lace from its first frame: compare against the tail that
		// starts at the first frame of block i's lace.
		start := i
		for start > 0 && at[start-1] == at[i] {
			start--
		}
		if !reflect.DeepEqual(got, want[start:]) {
			t.Fatalf("resume at block %d (%d blocks) differs from the full walk's tail (%d blocks)",
				i, len(got), len(want[start:]))
		}
	}
}

// A resumed walk must keep the timing context its cluster carries: the blocks
// it delivers are stamped exactly as the full walk stamped them (the cluster's
// Timestamp is not re-read from the file - it rides in the BlockPos).
func TestBlockReaderResumeKeepsClusterTiming(t *testing.T) {
	fixture := buildMixedBlockFixture(t, 3)
	full, err := NewBlockReader(bytes.NewReader(fixture), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	var want []mkv.Block
	var at []BlockPos
	for {
		b, err := full.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("full walk: %v", err)
		}
		want = append(want, b)
		at = append(at, full.Pos())
	}

	// A block in the LAST cluster: its timecode is only correct if the resumed
	// reader knows the cluster it sits in.
	last := len(want) - 1
	br, err := NewBlockReaderFrom(bytes.NewReader(fixture), 1_000_000, at[last])
	if err != nil {
		t.Fatal(err)
	}
	b, err := br.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b.Timecode != want[last].Timecode || b.BlockTimecode != want[last].BlockTimecode {
		t.Errorf("resumed block timecode %d (block %d), want %d (block %d): the cluster's timestamp was lost",
			b.Timecode, b.BlockTimecode, want[last].Timecode, want[last].BlockTimecode)
	}
}

// A zero BlockPos names no block: resuming on one must be refused, not read as
// an offset into the EBML header.
func TestBlockReaderResumeRejectsZeroPos(t *testing.T) {
	fixture := buildMixedBlockFixture(t, 1)
	if _, err := NewBlockReaderFrom(bytes.NewReader(fixture), 1_000_000, BlockPos{}); err == nil {
		t.Error("resuming at a zero BlockPos must fail")
	}
}
