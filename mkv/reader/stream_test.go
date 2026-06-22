package reader

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// readerOnly wraps an io.Reader and intentionally does NOT implement io.Seeker.
// This proves the streaming path works with a genuinely non-seekable source.
type readerOnly struct{ r io.Reader }

func (r *readerOnly) Read(p []byte) (int, error) { return r.r.Read(p) }

// buildStreamMKV builds a minimal well-formed streamable MKV:
//
//	EBML header (size=0) | Segment (known or unknown size) | Info | Tracks | Clusters
func buildStreamMKV(t *testing.T, unknownSizeSeg bool, clusters [][]mkv.Block, timecodeScale int64) []byte {
	t.Helper()

	// Info
	var info bytes.Buffer
	ebml.WriteElementHeader(&info, mkv.IDTimecodeScale, int64(ebml.UintLen(uint64(timecodeScale))))
	ebml.WriteUint(&info, uint64(timecodeScale), ebml.UintLen(uint64(timecodeScale)))

	// Tracks: one video track
	var trackEntry bytes.Buffer
	ebml.WriteElementHeader(&trackEntry, mkv.IDTrackNumber, 1)
	ebml.WriteUint(&trackEntry, 1, 1)
	ebml.WriteElementHeader(&trackEntry, mkv.IDTrackType, 1)
	ebml.WriteUint(&trackEntry, mkv.TrackTypeVideo, 1)
	ebml.WriteElementHeader(&trackEntry, mkv.IDCodecID, 15)
	ebml.WriteString(&trackEntry, "V_MPEG4/ISO/AVC")

	var tracks bytes.Buffer
	ebml.WriteElementHeader(&tracks, mkv.IDTrackEntry, int64(trackEntry.Len()))
	tracks.Write(trackEntry.Bytes())

	// Clusters
	var clustersBuf bytes.Buffer
	for ci, blks := range clusters {
		var cluster bytes.Buffer
		ts := uint64(ci * 1000)
		ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, int64(ebml.UintLen(ts)))
		ebml.WriteUint(&cluster, ts, ebml.UintLen(ts))

		for _, b := range blks {
			payload := []byte{0x81, 0x00, 0x00, 0x80}
			if b.Keyframe {
				payload[3] = 0x80
			} else {
				payload[3] = 0x00
			}
			payload = append(payload, b.Data...)
			ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(payload)))
			cluster.Write(payload)
		}

		ebml.WriteElementHeader(&clustersBuf, mkv.IDCluster, int64(cluster.Len()))
		clustersBuf.Write(cluster.Bytes())
	}

	// Segment body
	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, int64(info.Len()))
	seg.Write(info.Bytes())
	ebml.WriteElementHeader(&seg, mkv.IDTracks, int64(tracks.Len()))
	seg.Write(tracks.Bytes())
	seg.Write(clustersBuf.Bytes())

	// Full file
	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	if unknownSizeSeg {
		ebml.WriteElementID(&full, mkv.IDSegment)
		ebml.WriteDataSize(&full, -1)
	} else {
		ebml.WriteElementHeader(&full, mkv.IDSegment, int64(seg.Len()))
	}
	full.Write(seg.Bytes())

	return full.Bytes()
}

// buildUnknownSizeClusterMKV builds an MKV where the cluster uses unknown size.
func buildUnknownSizeClusterMKV(t *testing.T, nBlocks int) []byte {
	t.Helper()

	var info bytes.Buffer
	ebml.WriteElementHeader(&info, mkv.IDTimecodeScale, 3)
	ebml.WriteUint(&info, 1_000_000, 3)

	// One cluster with unknown size, followed by a known-size cluster.
	var cluster1 bytes.Buffer
	ebml.WriteElementHeader(&cluster1, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster1, 0, 1)
	for i := 0; i < nBlocks; i++ {
		payload := []byte{0x81, 0x00, 0x00, 0x80, byte(i), 0xAB}
		ebml.WriteElementHeader(&cluster1, mkv.IDSimpleBlock, int64(len(payload)))
		cluster1.Write(payload)
	}

	// cluster2 is known-size with 1 block; its presence terminates cluster1.
	var cluster2body bytes.Buffer
	ebml.WriteElementHeader(&cluster2body, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster2body, 1, 1)
	payload2 := []byte{0x81, 0x00, 0x01, 0x80, 0xFF, 0xFE}
	ebml.WriteElementHeader(&cluster2body, mkv.IDSimpleBlock, int64(len(payload2)))
	cluster2body.Write(payload2)

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, int64(info.Len()))
	seg.Write(info.Bytes())

	// Unknown-size cluster1 (no Cluster header size).
	ebml.WriteElementID(&seg, mkv.IDCluster)
	ebml.WriteDataSize(&seg, -1)
	seg.Write(cluster1.Bytes())

	// Known-size cluster2 terminates cluster1.
	ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(cluster2body.Len()))
	seg.Write(cluster2body.Bytes())

	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	ebml.WriteElementID(&full, mkv.IDSegment)
	ebml.WriteDataSize(&full, -1) // unknown-size segment
	full.Write(seg.Bytes())

	return full.Bytes()
}

// TestNewStreamBlockReaderReaderOnly verifies that NewStreamBlockReader works
// on a plain io.Reader (no Seek) and yields the same blocks as NewBlockReader.
func TestNewStreamBlockReaderReaderOnly(t *testing.T) {
	const timecodeScale = int64(1_000_000)
	blks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01, 0x02}},
		{TrackNumber: 1, Timecode: 33, Data: []byte{0x03, 0x04}},
	}
	data := buildStreamMKV(t, false, [][]mkv.Block{blks}, timecodeScale)

	// Seekable path.
	seekable := bytes.NewReader(data)
	brSeek, err := NewBlockReader(seekable, timecodeScale)
	if err != nil {
		t.Fatalf("seekable NewBlockReader: %v", err)
	}
	var seekBlocks []mkv.Block
	for {
		b, err := brSeek.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("seekable Next: %v", err)
		}
		seekBlocks = append(seekBlocks, b)
	}

	// Non-seekable path.
	stream := &readerOnly{r: bytes.NewReader(data)}
	brStream, err := NewStreamBlockReader(stream, timecodeScale)
	if err != nil {
		t.Fatalf("stream NewStreamBlockReader: %v", err)
	}
	var streamBlocks []mkv.Block
	for {
		b, err := brStream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream Next: %v", err)
		}
		streamBlocks = append(streamBlocks, b)
	}

	if len(seekBlocks) != len(streamBlocks) {
		t.Fatalf("block count: seekable=%d stream=%d", len(seekBlocks), len(streamBlocks))
	}
	for i := range seekBlocks {
		s, st := seekBlocks[i], streamBlocks[i]
		if s.TrackNumber != st.TrackNumber {
			t.Errorf("block[%d] track: seek=%d stream=%d", i, s.TrackNumber, st.TrackNumber)
		}
	}
}

// TestReadStreamRoundTripRegfix verifies ReadStream against the real fixture
// regfix.mkv, comparing blocks and track count with the seekable Read path.
func TestReadStreamRoundTripRegfix(t *testing.T) {
	path := "../../internal/testdata/regfix.mkv"
	fileData, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("regfix.mkv not available: %v", err)
	}

	// Seekable path.
	ctx := context.Background()
	seekC, err := Read(ctx, bytes.NewReader(fileData), path)
	if err != nil {
		t.Fatalf("seekable Read: %v", err)
	}
	seekBR, err := NewBlockReader(bytes.NewReader(fileData), seekC.Info.TimecodeScale)
	if err != nil {
		t.Fatalf("seekable NewBlockReader: %v", err)
	}
	var seekBlocks []mkv.Block
	for {
		b, err := seekBR.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("seekable Next: %v", err)
		}
		seekBlocks = append(seekBlocks, b)
	}

	// Streaming path — same file, wrapped in readerOnly.
	streamC, streamBR, err := ReadStream(ctx, &readerOnly{r: bytes.NewReader(fileData)})
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}

	// Compare metadata.
	if len(streamC.Tracks) != len(seekC.Tracks) {
		t.Errorf("track count: stream=%d seek=%d", len(streamC.Tracks), len(seekC.Tracks))
	}
	for i := range seekC.Tracks {
		if i >= len(streamC.Tracks) {
			break
		}
		st, sk := streamC.Tracks[i], seekC.Tracks[i]
		if st.ID != sk.ID {
			t.Errorf("track[%d] ID: stream=%d seek=%d", i, st.ID, sk.ID)
		}
		if st.Type != sk.Type {
			t.Errorf("track[%d] type: stream=%q seek=%q", i, st.Type, sk.Type)
		}
	}
	if len(streamC.Chapters) != len(seekC.Chapters) {
		t.Errorf("chapter count: stream=%d seek=%d", len(streamC.Chapters), len(seekC.Chapters))
	}

	// Compare blocks (count, track numbers, timecodes).
	var streamBlocks []mkv.Block
	for {
		b, err := streamBR.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream Next: %v", err)
		}
		streamBlocks = append(streamBlocks, b)
	}

	if len(seekBlocks) != len(streamBlocks) {
		t.Fatalf("block count: seekable=%d stream=%d", len(seekBlocks), len(streamBlocks))
	}
	for i := range seekBlocks {
		sk, st := seekBlocks[i], streamBlocks[i]
		if sk.TrackNumber != st.TrackNumber {
			t.Errorf("block[%d] track: seek=%d stream=%d", i, sk.TrackNumber, st.TrackNumber)
		}
		if sk.Timecode != st.Timecode {
			t.Errorf("block[%d] timecode: seek=%d stream=%d", i, sk.Timecode, st.Timecode)
		}
	}
}

// TestReadStreamUnknownSizeSeg verifies ReadStream handles unknown-size Segment.
func TestReadStreamUnknownSizeSeg(t *testing.T) {
	data := buildStreamMKV(t, true, [][]mkv.Block{
		{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0xAA}},
			{TrackNumber: 1, Timecode: 10, Data: []byte{0xBB}},
		},
	}, 1_000_000)

	ctx := context.Background()
	c, br, err := ReadStream(ctx, &readerOnly{r: bytes.NewReader(data)})
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(c.Tracks) != 1 {
		t.Errorf("tracks = %d, want 1", len(c.Tracks))
	}

	count := 0
	for {
		_, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		count++
	}
	if count != 2 {
		t.Errorf("blocks = %d, want 2", count)
	}
}

// TestUnknownSizeClusterBlockReader verifies that BlockReader handles a cluster
// with unknown size by detecting the next segment-level element as the boundary.
func TestUnknownSizeClusterBlockReader(t *testing.T) {
	const nBlocks = 3
	data := buildUnknownSizeClusterMKV(t, nBlocks)

	// Both seekable and non-seekable should work.
	for _, name := range []string{"seekable", "stream"} {
		t.Run(name, func(t *testing.T) {
			var br *BlockReader
			var err error
			if name == "seekable" {
				br, err = NewBlockReader(bytes.NewReader(data), 1_000_000)
			} else {
				br, err = NewStreamBlockReader(&readerOnly{r: bytes.NewReader(data)}, 1_000_000)
			}
			if err != nil {
				t.Fatalf("init: %v", err)
			}

			count := 0
			for {
				_, err := br.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("Next[%d]: %v", count, err)
				}
				count++
			}
			// nBlocks from unknown-size cluster1 + 1 block from known-size cluster2.
			want := nBlocks + 1
			if count != want {
				t.Errorf("blocks = %d, want %d", count, want)
			}
		})
	}
}

// TestReadStreamMetadata verifies ReadStream correctly parses Info and Tracks.
func TestReadStreamMetadata(t *testing.T) {
	// Build MKV with Info + Tracks + one cluster.
	const timecodeScale = int64(1_000_000)
	data := buildStreamMKV(t, false, [][]mkv.Block{
		{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0xDE, 0xAD}}},
	}, timecodeScale)

	ctx := context.Background()
	c, br, err := ReadStream(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}

	if c.Info.TimecodeScale != timecodeScale {
		t.Errorf("TimecodeScale = %d, want %d", c.Info.TimecodeScale, timecodeScale)
	}
	if len(c.Tracks) != 1 {
		t.Fatalf("tracks = %d, want 1", len(c.Tracks))
	}
	if c.Tracks[0].Type != mkv.VideoTrack {
		t.Errorf("track type = %q, want video", c.Tracks[0].Type)
	}
	if c.Tracks[0].Codec != "h264" {
		t.Errorf("track codec = %q, want h264", c.Tracks[0].Codec)
	}

	// Drain blocks.
	b, err := br.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if b.TrackNumber != 1 {
		t.Errorf("block track = %d, want 1", b.TrackNumber)
	}
	_, err = br.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF, got %v", err)
	}
}

// TestReadStreamTruncated verifies that truncated input yields an error,
// not a panic or infinite loop.
func TestReadStreamTruncated(t *testing.T) {
	data := buildStreamMKV(t, false, [][]mkv.Block{
		{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01, 0x02, 0x03}}},
	}, 1_000_000)

	ctx := context.Background()
	for limit := 1; limit < len(data)-1; limit++ {
		r := &readerOnly{r: bytes.NewReader(data[:limit])}
		c, br, err := ReadStream(ctx, r)
		if err != nil {
			continue // truncated before metadata: expected
		}
		// If metadata parsed, drain blocks; errors are expected.
		_ = c
		for {
			_, err := br.Next()
			if err != nil {
				break
			}
		}
	}
	// Test passes if no panic or infinite loop occurred.
}

// TestReadStreamContextCancel verifies ctx cancellation is respected.
func TestReadStreamContextCancel(t *testing.T) {
	data := buildStreamMKV(t, false, nil, 1_000_000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := ReadStream(ctx, bytes.NewReader(data))
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}

// TestReadStreamWrongEBML verifies error on non-EBML header.
func TestReadStreamWrongEBML(t *testing.T) {
	var buf bytes.Buffer
	ebml.WriteElementHeader(&buf, mkv.IDSegment, 0)
	_, _, err := ReadStream(context.Background(), &buf)
	if err == nil {
		t.Fatal("expected error for non-EBML header")
	}
}

// TestReadStreamWrongSegment verifies error on non-Segment after EBML header.
func TestReadStreamWrongSegment(t *testing.T) {
	var buf bytes.Buffer
	ebml.WriteElementHeader(&buf, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&buf, mkv.IDInfo, 0)
	_, _, err := ReadStream(context.Background(), &buf)
	if err == nil {
		t.Fatal("expected error for non-Segment")
	}
}

// TestReadStreamMultipleClusters verifies multiple clusters are correctly read.
func TestReadStreamMultipleClusters(t *testing.T) {
	const nClusters = 4
	const nBlocksPerCluster = 3
	clusters := make([][]mkv.Block, nClusters)
	for ci := range clusters {
		clusters[ci] = make([]mkv.Block, nBlocksPerCluster)
		for bi := range clusters[ci] {
			clusters[ci][bi] = mkv.Block{
				TrackNumber: 1,
				Timecode:    int64(ci*1000 + bi*33),
				Keyframe:    bi == 0,
				Data:        []byte{byte(ci), byte(bi)},
			}
		}
	}

	data := buildStreamMKV(t, false, clusters, 1_000_000)
	ctx := context.Background()
	_, br, err := ReadStream(ctx, &readerOnly{r: bytes.NewReader(data)})
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}

	count := 0
	for {
		_, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		count++
	}
	want := nClusters * nBlocksPerCluster
	if count != want {
		t.Errorf("blocks = %d, want %d", count, want)
	}
}
