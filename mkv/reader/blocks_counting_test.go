package reader

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func createTempFile(tb testing.TB) (*os.File, error) {
	tb.Helper()
	return os.CreateTemp("", "mkvgo-bench-*.mkv")
}

func removeTempFile(f *os.File) {
	f.Close()
	os.Remove(f.Name())
}

// buildMultiClusterMKV returns a bytes.Reader containing a minimal MKV with
// multiple clusters and enough blocks to exercise the full read path.
// The segment contains an Info element followed by numClusters clusters.
func buildMultiClusterMKV(t *testing.T, numClusters, blocksPerCluster int) *bytes.Reader {
	t.Helper()

	timecodeScale := uint64(1000000)

	// Build Info element (just TimecodeScale).
	var info bytes.Buffer
	ebml.WriteElementHeader(&info, mkv.IDTimecodeScale, int64(ebml.UintLen(timecodeScale)))
	ebml.WriteUint(&info, timecodeScale, ebml.UintLen(timecodeScale))

	// Build segment body: Info + clusters.
	var segBody bytes.Buffer
	ebml.WriteElementHeader(&segBody, mkv.IDInfo, int64(info.Len()))
	segBody.Write(info.Bytes())

	for c := 0; c < numClusters; c++ {
		var cluster bytes.Buffer
		ts := uint64(c * 1000)
		ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, int64(ebml.UintLen(ts)))
		ebml.WriteUint(&cluster, ts, ebml.UintLen(ts))

		for b := 0; b < blocksPerCluster; b++ {
			data := make([]byte, 32)
			blockPayload := []byte{0x81, 0x00, 0x00, 0x80}
			blockPayload = append(blockPayload, data...)
			ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(blockPayload)))
			cluster.Write(blockPayload)
		}

		ebml.WriteElementHeader(&segBody, mkv.IDCluster, int64(cluster.Len()))
		segBody.Write(cluster.Bytes())
	}

	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&full, mkv.IDSegment, int64(segBody.Len()))
	full.Write(segBody.Bytes())

	return bytes.NewReader(full.Bytes())
}

// TestMultiClusterBlockCount verifies that BlockReader reads the correct
// total number of blocks across multiple clusters.
func TestMultiClusterBlockCount(t *testing.T) {
	const numClusters = 5
	const blocksPerCluster = 20
	r := buildMultiClusterMKV(t, numClusters, blocksPerCluster)
	br, err := NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	got := 0
	for {
		_, err := br.Next()
		if err != nil {
			break
		}
		got++
	}
	want := numClusters * blocksPerCluster
	if got != want {
		t.Errorf("got %d blocks, want %d", got, want)
	}
}

// TestCounterMatchesRealOffset verifies that after parsing, the counting
// position matches what the underlying io.ReadSeeker reports.
func TestCounterMatchesRealOffset(t *testing.T) {
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)

	for i := 0; i < 10; i++ {
		blockPayload := []byte{0x81, 0x00, 0x00, 0x80, 0xDE, 0xAD, 0xBE, 0xEF}
		ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(blockPayload)))
		cluster.Write(blockPayload)
	}

	r := buildBlockReaderInput(cluster.Bytes())
	br, err := NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	for {
		_, err := br.Next()
		if err != nil {
			break
		}
		// Counter must match real file position after every block.
		realPos, seekErr := r.Seek(0, io.SeekCurrent)
		if seekErr != nil {
			t.Fatalf("seek: %v", seekErr)
		}
		counterPos := br.r.tell()
		// The bufio.Reader may have read-ahead bytes not yet "consumed" by the
		// caller, so the real file position can be ahead of the counting pos.
		// What we verify: counter <= realPos (counter never lies about how many
		// bytes the caller has logically consumed) and counter is within the
		// buffer window of realPos.
		if counterPos > realPos {
			t.Errorf("counter %d > real file pos %d: counter must not exceed real pos", counterPos, realPos)
		}
		if realPos-counterPos > bufSize {
			t.Errorf("read-ahead gap %d exceeds buffer size %d", realPos-counterPos, bufSize)
		}
	}

	// After draining, counter should equal what the underlying seeker reports
	// (the bufio.Reader may have buffered bytes that will never be read —
	// so the real seeked pos can be >= counter). Verify counter is sane.
	if br.r.tell() <= 0 {
		t.Error("counter is zero after reading all blocks")
	}
}

// TestCounterCorrectAfterInit verifies the counter is seeded to the exact
// position right after the EBML+Segment headers — no off-by-one.
func TestCounterCorrectAfterInit(t *testing.T) {
	// Build a known-size payload.
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)
	blockPayload := []byte{0x81, 0x00, 0x00, 0x80, 0xFF}
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(blockPayload)))
	cluster.Write(blockPayload)

	// buildBlockReaderInput constructs: [EBMLHdr(size=0)] [Segment(size=X)] [Cluster…]
	// EBMLHdr element = 4 bytes ID + 1 byte size(0) = 5 bytes header + 0 body = 5 bytes.
	// Segment element header = 4 bytes ID + variable size encoding.
	r := buildBlockReaderInput(cluster.Bytes())

	// Record position by seeking BEFORE creating the BlockReader.
	startReal, _ := r.Seek(0, io.SeekCurrent) // should be 0
	_ = startReal

	br, err := NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// After init, counter equals bytes consumed from file:
	//   n1 (ebml header bytes) + h1.Size (ebml body) + n2 (segment header bytes)
	// Verify the real file position by seeking.
	realAfterInit, _ := r.Seek(0, io.SeekCurrent)
	counterAfterInit := br.r.tell()

	// The bufio.Reader issues a read-ahead on first use. Since we haven't
	// called Next() yet, no read-ahead has happened, and the real position
	// must equal the counter.
	if counterAfterInit != realAfterInit {
		t.Errorf("counter after init = %d, real pos = %d; want equal", counterAfterInit, realAfterInit)
	}
}

// TestLargeSkipViaDiscard builds a segment with a large non-cluster element
// (simulating Attachments) and verifies the reader skips it correctly.
func TestLargeSkipViaDiscard(t *testing.T) {
	// Build the segment body manually:
	//   [Info element]
	//   [large Attachments-like element]
	//   [Cluster with one block]
	largePayload := make([]byte, 512*1024) // 512 KiB — larger than the 256 KiB buffer
	for i := range largePayload {
		largePayload[i] = 0xAB
	}

	var seg bytes.Buffer
	// A non-cluster element with known size: use IDInfo (won't be parsed by
	// BlockReader, it will just skip it).
	ebml.WriteElementHeader(&seg, mkv.IDInfo, int64(len(largePayload)))
	seg.Write(largePayload)

	// Now add a cluster with one block.
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)
	blockPayload := []byte{0x81, 0x00, 0x00, 0x80, 0xDE, 0xAD}
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(blockPayload)))
	cluster.Write(blockPayload)
	ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(cluster.Len()))
	seg.Write(cluster.Bytes())

	// Wrap in EBML+Segment.
	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&full, mkv.IDSegment, int64(seg.Len()))
	full.Write(seg.Bytes())

	r := bytes.NewReader(full.Bytes())
	br, err := NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// The first Next() must skip the large Info element and return a block.
	b, err := br.Next()
	if err != nil {
		t.Fatalf("expected a block after large skip, got: %v", err)
	}
	if len(b.Data) != 2 {
		t.Errorf("block data len = %d, want 2", len(b.Data))
	}
	// Verify counter is in the right ballpark.
	expectedMin := int64(len(largePayload)) // must have consumed at least the large payload
	if br.r.tell() < expectedMin {
		t.Errorf("counter %d < expected min %d after large skip", br.r.tell(), expectedMin)
	}

	// Second Next() must return EOF.
	_, err = br.Next()
	if err != io.EOF {
		t.Errorf("expected EOF after only block, got: %v", err)
	}
}

// TestTruncatedDiscardGraceful verifies that a truncated stream during a
// large discard does not panic and returns an error (not EOF confusion).
func TestTruncatedDiscardGraceful(t *testing.T) {
	largePayload := make([]byte, 1024)

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, int64(len(largePayload)))
	seg.Write(largePayload[:512]) // truncate: only write half the declared payload

	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&full, mkv.IDSegment, int64(seg.Len()))
	full.Write(seg.Bytes())

	r := bytes.NewReader(full.Bytes())
	br, err := NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Must error (not panic, not infinite loop).
	_, err = br.Next()
	if err == nil {
		t.Fatal("expected error for truncated large-skip element")
	}
}

// BenchmarkBlockReaderNext measures throughput of the buffered reader.
// Run with: go test ./mkv/reader/... -bench=BenchmarkBlockReaderNext -benchmem
func BenchmarkBlockReaderNext(b *testing.B) {
	// Build a fixture with many clusters and blocks.
	const numClusters = 50
	const blocksPerCluster = 100
	const blockDataSize = 4096

	var seg bytes.Buffer
	for c := 0; c < numClusters; c++ {
		var cluster bytes.Buffer
		ts := uint64(c * 1000)
		ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, int64(ebml.UintLen(ts)))
		ebml.WriteUint(&cluster, ts, ebml.UintLen(ts))
		for bk := 0; bk < blocksPerCluster; bk++ {
			data := make([]byte, blockDataSize)
			blockPayload := []byte{0x81, 0x00, 0x00, 0x80}
			blockPayload = append(blockPayload, data...)
			ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(blockPayload)))
			cluster.Write(blockPayload)
		}
		ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(cluster.Len()))
		seg.Write(cluster.Bytes())
	}

	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&full, mkv.IDSegment, int64(seg.Len()))
	full.Write(seg.Bytes())
	fixture := full.Bytes()

	b.SetBytes(int64(len(fixture)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := bytes.NewReader(fixture)
		br, err := NewBlockReader(r, 1000000)
		if err != nil {
			b.Fatal(err)
		}
		for {
			_, err := br.Next()
			if err != nil {
				break
			}
		}
	}
}

// BenchmarkBlockReaderOnSampleMKV runs over the real sample fixture if
// available, exercising the discard path for non-cluster elements.
func BenchmarkBlockReaderOnSampleMKV(b *testing.B) {
	// Build a more realistic fixture using the writer package so the Cues /
	// SeekHead skips are exercised. MKVWriter requires an io.WriteSeeker so
	// we use a temporary file.
	track := mkv.Track{
		ID:    1,
		Type:  mkv.VideoTrack,
		Codec: "h264",
	}
	blocks := make([]mkv.Block, 200)
	for i := range blocks {
		blocks[i] = mkv.Block{
			TrackNumber: 1,
			Timecode:    int64(i) * 33,
			Keyframe:    i == 0,
			Data:        make([]byte, 2048),
		}
	}

	f, err := createTempFile(b)
	if err != nil {
		b.Fatal(err)
	}
	defer removeTempFile(f)

	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		b.Fatal(err)
	}
	cont := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1000000}}
	if err := mw.WriteMetadata(cont, []mkv.Track{track}, 6600); err != nil {
		b.Fatal(err)
	}
	if err := mw.WriteClusterWithCues(0, 1000000, blocks); err != nil {
		b.Fatal(err)
	}
	if err := mw.Finalize(); err != nil {
		b.Fatal(err)
	}

	// Read all bytes into memory so the benchmark doesn't measure disk I/O.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		b.Fatal(err)
	}
	fixtureBytes, err := io.ReadAll(f)
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(int64(len(fixtureBytes)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := bytes.NewReader(fixtureBytes)
		br, err := NewBlockReader(r, 1000000)
		if err != nil {
			b.Fatal(err)
		}
		for {
			_, err := br.Next()
			if err != nil {
				break
			}
		}
	}
}
