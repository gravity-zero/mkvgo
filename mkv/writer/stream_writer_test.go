package writer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestStreamWriterRoundTrip writes a live stream to a bytes.Buffer and reads
// it back with the streaming reader, verifying tracks and blocks match.
func TestStreamWriterRoundTrip(t *testing.T) {
	const timecodeScale = int64(1_000_000)
	sr := 48000.0
	ch := uint8(2)
	w := uint32(1920)
	h := uint32(1080)

	info := mkv.SegmentInfo{
		TimecodeScale: timecodeScale,
		MuxingApp:     "mkvgo-stream-test",
		WritingApp:    "mkvgo-stream-test",
	}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng", IsDefault: true, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "opus", Language: "eng", SampleRate: &sr, Channels: &ch},
	}

	// Input blocks: 3 video (keyframe at 0, P-frames at 33/66) + 3 audio.
	inputBlocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01, 0x02, 0x03}},
		{TrackNumber: 2, Timecode: 0, Keyframe: false, Data: []byte{0xAA, 0xBB}},
		{TrackNumber: 1, Timecode: 33, Keyframe: false, Data: []byte{0x04, 0x05}},
		{TrackNumber: 2, Timecode: 20, Keyframe: false, Data: []byte{0xCC, 0xDD}},
		{TrackNumber: 1, Timecode: 66, Keyframe: true, Data: []byte{0x06, 0x07}},
		{TrackNumber: 2, Timecode: 40, Keyframe: false, Data: []byte{0xEE, 0xFF}},
	}

	// Write.
	var buf bytes.Buffer
	sw, err := NewStreamWriter(&buf, info, tracks)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	for _, b := range inputBlocks {
		if err := sw.WriteBlock(b); err != nil {
			t.Fatalf("WriteBlock: %v", err)
		}
	}
	if err := sw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data := buf.Bytes()

	// Verify unknown-size Segment is present.
	// The Segment element ID (0x18538067) = 4 bytes, followed by 8-byte unknown-size VINT.
	if !hasUnknownSizeSegment(data) {
		t.Error("output does not contain an unknown-size Segment")
	}

	// Read back via ReadStream.
	ctx := context.Background()
	c, br, err := reader.ReadStream(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}

	// Verify tracks.
	if len(c.Tracks) != len(tracks) {
		t.Errorf("track count: got %d, want %d", len(c.Tracks), len(tracks))
	}
	for i, want := range tracks {
		if i >= len(c.Tracks) {
			break
		}
		got := c.Tracks[i]
		if got.ID != want.ID {
			t.Errorf("track[%d] ID: got %d, want %d", i, got.ID, want.ID)
		}
		if got.Type != want.Type {
			t.Errorf("track[%d] type: got %q, want %q", i, got.Type, want.Type)
		}
		if got.Language != want.Language {
			t.Errorf("track[%d] language: got %q, want %q", i, got.Language, want.Language)
		}
	}

	// Drain and count blocks.
	var gotBlocks []mkv.Block
	for {
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		gotBlocks = append(gotBlocks, b)
	}

	if len(gotBlocks) != len(inputBlocks) {
		t.Fatalf("block count: got %d, want %d", len(gotBlocks), len(inputBlocks))
	}
	for i, want := range inputBlocks {
		got := gotBlocks[i]
		if got.TrackNumber != want.TrackNumber {
			t.Errorf("block[%d] track: got %d, want %d", i, got.TrackNumber, want.TrackNumber)
		}
		if !bytes.Equal(got.Data, want.Data) {
			t.Errorf("block[%d] data mismatch: got %x, want %x", i, got.Data, want.Data)
		}
	}
}

// hasUnknownSizeSegment checks that the output contains an unknown-size Segment
// element (IDSegment followed by 0x01FFFFFFFFFFFFFF).
func hasUnknownSizeSegment(data []byte) bool {
	// IDSegment = 0x18538067 (4 bytes), then 8-byte unknown-size VINT.
	unknownSize := [8]byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	id := [4]byte{0x18, 0x53, 0x80, 0x67}
	for i := 0; i+12 <= len(data); i++ {
		if data[i] == id[0] && data[i+1] == id[1] && data[i+2] == id[2] && data[i+3] == id[3] {
			if i+12 <= len(data) {
				var got [8]byte
				copy(got[:], data[i+4:i+12])
				if got == unknownSize {
					return true
				}
			}
		}
	}
	return false
}

// hasNoSeekHead verifies no SeekHead element exists in the output.
func hasNoSeekHead(data []byte) bool {
	// IDSeekHead = 0x114D9B74 (4 bytes).
	id := [4]byte{0x11, 0x4D, 0x9B, 0x74}
	for i := 0; i+4 <= len(data); i++ {
		if data[i] == id[0] && data[i+1] == id[1] && data[i+2] == id[2] && data[i+3] == id[3] {
			return false
		}
	}
	return true
}

// hasNoCues verifies no Cues element exists in the output.
func hasNoCues(data []byte) bool {
	// IDCues = 0x1C53BB6B (4 bytes).
	id := [4]byte{0x1C, 0x53, 0xBB, 0x6B}
	for i := 0; i+4 <= len(data); i++ {
		if data[i] == id[0] && data[i+1] == id[1] && data[i+2] == id[2] && data[i+3] == id[3] {
			return false
		}
	}
	return true
}

// TestStreamWriterNoSeekHeadNoCues verifies the output contains neither
// SeekHead nor Cues (both require seek capability).
func TestStreamWriterNoSeekHeadNoCues(t *testing.T) {
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true},
	}

	var buf bytes.Buffer
	sw, err := NewStreamWriter(&buf, info, tracks)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	if err := sw.WriteBlock(mkv.Block{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01}}); err != nil {
		t.Fatalf("WriteBlock: %v", err)
	}
	sw.Close()

	data := buf.Bytes()
	if !hasNoSeekHead(data) {
		t.Error("output contains SeekHead (should not for live stream)")
	}
	if !hasNoCues(data) {
		t.Error("output contains Cues (should not for live stream)")
	}
}

// TestStreamWriterUnknownSizeClusters verifies each cluster in the output uses
// unknown-size framing.
func TestStreamWriterUnknownSizeClusters(t *testing.T) {
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true},
	}

	var buf bytes.Buffer
	sw, err := NewStreamWriter(&buf, info, tracks)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	// Write two clusters (keyframes force cluster boundaries).
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01}},
		{TrackNumber: 1, Timecode: 33, Keyframe: false, Data: []byte{0x02}},
		{TrackNumber: 1, Timecode: 100, Keyframe: true, Data: []byte{0x03}}, // new cluster
	}
	for _, b := range blocks {
		if err := sw.WriteBlock(b); err != nil {
			t.Fatalf("WriteBlock: %v", err)
		}
	}
	sw.Close()

	data := buf.Bytes()
	// IDCluster = 0x1F43B675.
	clusterID := [4]byte{0x1F, 0x43, 0xB6, 0x75}
	unknownSize := [8]byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

	clusterCount := 0
	for i := 0; i+12 <= len(data); i++ {
		if data[i] == clusterID[0] && data[i+1] == clusterID[1] &&
			data[i+2] == clusterID[2] && data[i+3] == clusterID[3] {
			var got [8]byte
			copy(got[:], data[i+4:i+12])
			if got != unknownSize {
				t.Errorf("cluster at offset %d has known size (should be unknown)", i)
			}
			clusterCount++
		}
	}
	if clusterCount != 2 {
		t.Errorf("cluster count = %d, want 2", clusterCount)
	}
}

// TestStreamWriterFlushCluster verifies FlushCluster forces a new cluster
// even for non-keyframe blocks.
func TestStreamWriterFlushCluster(t *testing.T) {
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.AudioTrack, Codec: "opus", IsDefault: true},
	}

	var buf bytes.Buffer
	sw, err := NewStreamWriter(&buf, info, tracks)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}

	// All blocks are non-keyframe; FlushCluster forces the boundary.
	sw.WriteBlock(mkv.Block{TrackNumber: 1, Timecode: 0, Data: []byte{0xAA}})
	sw.WriteBlock(mkv.Block{TrackNumber: 1, Timecode: 20, Data: []byte{0xBB}})
	sw.FlushCluster()
	sw.WriteBlock(mkv.Block{TrackNumber: 1, Timecode: 40, Data: []byte{0xCC}})
	sw.Close()

	data := buf.Bytes()
	clusterID := [4]byte{0x1F, 0x43, 0xB6, 0x75}
	clusterCount := 0
	for i := 0; i+4 <= len(data); i++ {
		if data[i] == clusterID[0] && data[i+1] == clusterID[1] &&
			data[i+2] == clusterID[2] && data[i+3] == clusterID[3] {
			clusterCount++
		}
	}
	if clusterCount != 2 {
		t.Errorf("cluster count = %d, want 2 (after FlushCluster)", clusterCount)
	}

	// Round-trip.
	ctx := context.Background()
	_, br, err := reader.ReadStream(ctx, bytes.NewReader(data))
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
	if count != 3 {
		t.Errorf("block count = %d, want 3", count)
	}
}

// TestStreamWriterDefaultTimecodeScale verifies that a zero TimecodeScale
// is replaced with the default 1_000_000.
func TestStreamWriterDefaultTimecodeScale(t *testing.T) {
	info := mkv.SegmentInfo{TimecodeScale: 0} // zero → default
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true},
	}

	var buf bytes.Buffer
	sw, err := NewStreamWriter(&buf, info, tracks)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	sw.WriteBlock(mkv.Block{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01}})
	sw.Close()

	ctx := context.Background()
	c, _, err := reader.ReadStream(ctx, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if c.Info.TimecodeScale != 1_000_000 {
		t.Errorf("TimecodeScale = %d, want 1_000_000", c.Info.TimecodeScale)
	}
}

// TestStreamWriterEmptyTracksNoError verifies that writing zero tracks succeeds.
func TestStreamWriterEmptyTracksNoError(t *testing.T) {
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	var buf bytes.Buffer
	sw, err := NewStreamWriter(&buf, info, nil)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	if err := sw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestStreamWriterWriteBlockInCurrentCluster verifies that keyframe blocks do
// not trigger a new cluster when written via WriteBlockInCurrentCluster.
func TestStreamWriterWriteBlockInCurrentCluster(t *testing.T) {
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true}}

	var buf bytes.Buffer
	sw, err := NewStreamWriter(&buf, info, tracks)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}

	// Write two keyframes in the same cluster.
	sw.WriteBlockInCurrentCluster(mkv.Block{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01}})
	sw.WriteBlockInCurrentCluster(mkv.Block{TrackNumber: 1, Timecode: 33, Keyframe: true, Data: []byte{0x02}})
	sw.Close()

	data := buf.Bytes()
	clusterID := [4]byte{0x1F, 0x43, 0xB6, 0x75}
	clusterCount := 0
	for i := 0; i+4 <= len(data); i++ {
		if data[i] == clusterID[0] && data[i+1] == clusterID[1] &&
			data[i+2] == clusterID[2] && data[i+3] == clusterID[3] {
			clusterCount++
		}
	}
	if clusterCount != 1 {
		t.Errorf("cluster count = %d, want 1 (both keyframes in same cluster)", clusterCount)
	}
}

// TestStreamWriterRoundTripReadStream is the key round-trip: write via
// StreamWriter, read back via ReadStream (the commit-1 API), verify identity.
func TestStreamWriterRoundTripReadStream(t *testing.T) {
	const timecodeScale = int64(1_000_000)
	info := mkv.SegmentInfo{
		TimecodeScale: timecodeScale,
		MuxingApp:     "mkvgo",
		WritingApp:    "mkvgo",
	}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng", IsDefault: true},
	}

	// Build 3 clusters of 2 blocks each.
	allBlocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x00}},
		{TrackNumber: 1, Timecode: 33, Keyframe: false, Data: []byte{0x01}},
		{TrackNumber: 1, Timecode: 100, Keyframe: true, Data: []byte{0x02}},
		{TrackNumber: 1, Timecode: 133, Keyframe: false, Data: []byte{0x03}},
		{TrackNumber: 1, Timecode: 200, Keyframe: true, Data: []byte{0x04}},
		{TrackNumber: 1, Timecode: 233, Keyframe: false, Data: []byte{0x05}},
	}

	var buf bytes.Buffer
	sw, err := NewStreamWriter(&buf, info, tracks)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	for _, b := range allBlocks {
		if err := sw.WriteBlock(b); err != nil {
			t.Fatalf("WriteBlock: %v", err)
		}
	}
	sw.Close()

	ctx := context.Background()
	c, br, err := reader.ReadStream(ctx, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}

	if c.Info.TimecodeScale != timecodeScale {
		t.Errorf("TimecodeScale = %d", c.Info.TimecodeScale)
	}
	if len(c.Tracks) != 1 || c.Tracks[0].Codec != "h264" {
		t.Errorf("tracks mismatch: %+v", c.Tracks)
	}

	var got []mkv.Block
	for {
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, b)
	}

	if len(got) != len(allBlocks) {
		t.Fatalf("block count: got %d want %d", len(got), len(allBlocks))
	}
	for i, want := range allBlocks {
		if got[i].TrackNumber != want.TrackNumber {
			t.Errorf("block[%d] track: got %d want %d", i, got[i].TrackNumber, want.TrackNumber)
		}
		if !bytes.Equal(got[i].Data, want.Data) {
			t.Errorf("block[%d] data: got %x want %x", i, got[i].Data, want.Data)
		}
	}
}

// TestStreamWriterWriteError verifies that a write error is propagated.
func TestStreamWriterWriteError(t *testing.T) {
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true}}

	// alwaysErrWriter always errors on Write.
	ew := &alwaysErrWriter{}
	_, err := NewStreamWriter(ew, info, tracks)
	if err == nil {
		t.Fatal("expected error from alwaysErrWriter")
	}
}

type alwaysErrWriter struct{}

func (e *alwaysErrWriter) Write(p []byte) (int, error) {
	return 0, io.ErrClosedPipe
}

// TestStreamWriterLargeClusters verifies that many blocks across many clusters
// round-trip correctly.
func TestStreamWriterLargeClusters(t *testing.T) {
	const nClusters = 10
	const nBlocksPerCluster = 5

	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true}}

	var allBlocks []mkv.Block
	for ci := 0; ci < nClusters; ci++ {
		for bi := 0; bi < nBlocksPerCluster; bi++ {
			allBlocks = append(allBlocks, mkv.Block{
				TrackNumber: 1,
				Timecode:    int64(ci*1000 + bi*33),
				Keyframe:    bi == 0,
				Data:        []byte{byte(ci), byte(bi)},
			})
		}
	}

	var buf bytes.Buffer
	sw, err := NewStreamWriter(&buf, info, tracks)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	for _, b := range allBlocks {
		if err := sw.WriteBlock(b); err != nil {
			t.Fatalf("WriteBlock: %v", err)
		}
	}
	sw.Close()

	ctx := context.Background()
	_, br, err := reader.ReadStream(ctx, bytes.NewReader(buf.Bytes()))
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
	if count != len(allBlocks) {
		t.Errorf("block count: got %d want %d", count, len(allBlocks))
	}
}

// TestStreamWriterBlockReaderCanReadStream verifies the seekable BlockReader
// (which only processes Cluster elements) can read blocks from a stream output.
// reader.Read (the metadata parser) cannot handle unknown-size Clusters in its
// segment-level loop — that is expected. stream output targets ReadStream.
// NewBlockReader works because it uses inCluster/peeked to handle them.
func TestStreamWriterBlockReaderCanReadStream(t *testing.T) {
	w := uint32(1280)
	h := uint32(720)
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng", IsDefault: true, Width: &w, Height: &h},
	}

	inputBlocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0xDE, 0xAD}},
		{TrackNumber: 1, Timecode: 33, Keyframe: false, Data: []byte{0xBE, 0xEF}},
	}

	var buf bytes.Buffer
	sw, err := NewStreamWriter(&buf, info, tracks)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	for _, b := range inputBlocks {
		sw.WriteBlock(b)
	}
	sw.Close()

	// NewBlockReader skips non-cluster elements (Info, Tracks) via discard and
	// handles unknown-size clusters, so it works on stream output.
	br, err := reader.NewBlockReader(bytes.NewReader(buf.Bytes()), 1_000_000)
	if err != nil {
		t.Fatalf("NewBlockReader: %v", err)
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
	if count != len(inputBlocks) {
		t.Errorf("block count = %d, want %d", count, len(inputBlocks))
	}
}

// TestStreamWriterVerifyUnknownSizeSegmentAndClusters is the structural
// verification: ensures both the Segment and all Clusters use unknown-size.
func TestStreamWriterVerifyUnknownSizeSegmentAndClusters(t *testing.T) {
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true}}

	var buf bytes.Buffer
	sw, err := NewStreamWriter(&buf, info, tracks)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	// 3 clusters.
	for _, b := range []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01}},
		{TrackNumber: 1, Timecode: 100, Keyframe: true, Data: []byte{0x02}},
		{TrackNumber: 1, Timecode: 200, Keyframe: true, Data: []byte{0x03}},
	} {
		sw.WriteBlock(b)
	}
	sw.Close()

	data := buf.Bytes()
	unknownSize := [8]byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

	// Check Segment unknown size.
	if !hasUnknownSizeSegment(data) {
		t.Error("Segment does not use unknown-size framing")
	}

	// Check each Cluster has unknown size.
	clusterID := [4]byte{0x1F, 0x43, 0xB6, 0x75}
	for i := 0; i+12 <= len(data); i++ {
		if data[i] == clusterID[0] && data[i+1] == clusterID[1] &&
			data[i+2] == clusterID[2] && data[i+3] == clusterID[3] {
			var got [8]byte
			copy(got[:], data[i+4:i+12])
			if got != unknownSize {
				t.Errorf("Cluster at byte %d does not use unknown-size framing", i)
			}
		}
	}
}

// TestStreamWriterTimecodeOverflow verifies that a block whose timecode is more
// than int16 milliseconds from the cluster start is rejected with an error,
// instead of silently wrapping (a SimpleBlock's relative timecode is a signed
// 16-bit field, so an out-of-range cast would corrupt the timestamp).
func TestStreamWriterTimecodeOverflow(t *testing.T) {
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	// Audio-only stream: no keyframes, so WriteBlock never reopens a cluster and
	// the relative timecode grows without bound — the exact at-risk case.
	tracks := []mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "opus", IsDefault: true}}

	newSW := func() *StreamWriter {
		var buf bytes.Buffer
		sw, err := NewStreamWriter(&buf, info, tracks)
		if err != nil {
			t.Fatalf("NewStreamWriter: %v", err)
		}
		return sw
	}

	// First block opens the cluster at ts=0; a later non-keyframe block +32768 ms
	// away must error (before the fix it wrapped to -32768).
	t.Run("WriteBlock past int16 max", func(t *testing.T) {
		sw := newSW()
		if err := sw.WriteBlock(mkv.Block{TrackNumber: 1, Timecode: 0, Data: []byte{0xAA}}); err != nil {
			t.Fatalf("first WriteBlock: %v", err)
		}
		if err := sw.WriteBlock(mkv.Block{TrackNumber: 1, Timecode: 32768, Data: []byte{0xBB}}); err == nil {
			t.Fatal("WriteBlock(+32768ms): expected overflow error, got nil")
		}
	})

	// Exactly +32767 ms is the largest offset that still fits — must succeed.
	t.Run("WriteBlock at int16 max", func(t *testing.T) {
		sw := newSW()
		if err := sw.WriteBlock(mkv.Block{TrackNumber: 1, Timecode: 0, Data: []byte{0xAA}}); err != nil {
			t.Fatalf("first WriteBlock: %v", err)
		}
		if err := sw.WriteBlock(mkv.Block{TrackNumber: 1, Timecode: 32767, Data: []byte{0xBB}}); err != nil {
			t.Fatalf("WriteBlock(+32767ms): unexpected error: %v", err)
		}
	})

	// A far keyframe via WriteBlockInCurrentCluster does not reopen a cluster, so
	// it too must error rather than wrap.
	t.Run("WriteBlockInCurrentCluster past int16 max", func(t *testing.T) {
		sw := newSW()
		if err := sw.WriteBlockInCurrentCluster(mkv.Block{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0xAA}}); err != nil {
			t.Fatalf("first block: %v", err)
		}
		if err := sw.WriteBlockInCurrentCluster(mkv.Block{TrackNumber: 1, Timecode: 40000, Keyframe: true, Data: []byte{0xBB}}); err == nil {
			t.Fatal("WriteBlockInCurrentCluster(+40000ms): expected overflow error, got nil")
		}
	})

	// Negative overflow: a block far before the cluster start must also error.
	t.Run("past int16 min", func(t *testing.T) {
		sw := newSW()
		if err := sw.WriteBlock(mkv.Block{TrackNumber: 1, Timecode: 100000, Data: []byte{0xAA}}); err != nil {
			t.Fatalf("first WriteBlock: %v", err)
		}
		if err := sw.WriteBlockInCurrentCluster(mkv.Block{TrackNumber: 1, Timecode: 0, Data: []byte{0xBB}}); err == nil {
			t.Fatal("WriteBlockInCurrentCluster(-100000ms): expected overflow error, got nil")
		}
	})
}
