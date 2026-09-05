package reader

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// countingSeeker counts the bytes and the calls actually read from the
// source - the probes that prove a filtered walk skips payloads instead of
// reading them, and reads in bulk chunks instead of per block (each source
// read is a round trip on a network filesystem).
type countingSeeker struct {
	r     *bytes.Reader
	n     int64
	calls int64
}

func (c *countingSeeker) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	c.calls++
	return n, err
}
func (c *countingSeeker) Seek(offset int64, whence int) (int64, error) {
	return c.r.Seek(offset, whence)
}

// writeSimpleBlock appends a SimpleBlock for a small track number.
func writeSimpleBlock(cluster *bytes.Buffer, track byte, relTC int16, flags byte, data []byte) {
	payload := []byte{0x80 | track, byte(uint16(relTC) >> 8), byte(uint16(relTC)), flags}
	payload = append(payload, data...)
	ebml.WriteElementHeader(cluster, mkv.IDSimpleBlock, int64(len(payload)))
	cluster.Write(payload)
}

// writeBlockGroup appends a BlockGroup (Block + optional BlockDuration).
func writeBlockGroup(cluster *bytes.Buffer, track byte, relTC int16, data []byte, durationMs int64) {
	block := []byte{0x80 | track, byte(uint16(relTC) >> 8), byte(uint16(relTC)), 0x00}
	block = append(block, data...)
	var group bytes.Buffer
	ebml.WriteElementHeader(&group, mkv.IDBlock, int64(len(block)))
	group.Write(block)
	if durationMs > 0 {
		ebml.WriteElementHeader(&group, mkv.IDBlockDuration, int64(ebml.UintLen(uint64(durationMs))))
		ebml.WriteUint(&group, uint64(durationMs), ebml.UintLen(uint64(durationMs)))
	}
	ebml.WriteElementHeader(cluster, mkv.IDBlockGroup, int64(group.Len()))
	cluster.Write(group.Bytes())
}

// buildTwoTrackFixture builds a segment mixing a heavy track 1 (256 KiB
// payloads - comfortably above the seek threshold: SimpleBlocks and a
// BlockGroup) with a light track 2 (small text payloads: SimpleBlocks, an
// Xiph-laced block and a BlockGroup with duration).
func buildTwoTrackFixture(t *testing.T) []byte {
	t.Helper()
	heavy := make([]byte, 256<<10)
	for i := range heavy {
		heavy[i] = byte(i)
	}

	var seg bytes.Buffer
	for c := 0; c < 4; c++ {
		var cluster bytes.Buffer
		ts := uint64(c * 1000)
		ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, int64(ebml.UintLen(ts)))
		ebml.WriteUint(&cluster, ts, ebml.UintLen(ts))

		for b := 0; b < 8; b++ {
			writeSimpleBlock(&cluster, 1, int16(b*40), 0x80, heavy)
			if b == 3 {
				writeSimpleBlock(&cluster, 2, int16(b*40), 0x80, []byte("piste2 simple"))
			}
			if b == 5 {
				writeBlockGroup(&cluster, 2, int16(b*40), []byte("piste2 groupe"), 1500)
			}
		}
		// A heavy BlockGroup on track 1: the filter must skip the whole group.
		writeBlockGroup(&cluster, 1, 900, heavy, 40)
		// An Xiph-laced track-2 block: two frames, one lacing header byte.
		f0, f1 := []byte("lace-frame-0"), []byte("lace-frame-1!")
		laced := []byte{0x82, 0x03, 0x84, 0x80 | (lacingXiph << 1), 0x01, byte(len(f0))}
		laced = append(laced, f0...)
		laced = append(laced, f1...)
		ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(laced)))
		cluster.Write(laced)

		ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(cluster.Len()))
		seg.Write(cluster.Bytes())
	}

	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&full, mkv.IDSegment, int64(seg.Len()))
	full.Write(seg.Bytes())
	return full.Bytes()
}

func drain(t *testing.T, br *BlockReader) []mkv.Block {
	t.Helper()
	var out []mkv.Block
	for {
		b, err := br.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, b)
	}
}

// A track-filtered walk must return exactly the blocks a full walk returns
// for that track - same order, timecodes, keyframes, durations, data  -
// across SimpleBlocks, laced blocks and BlockGroups.
func TestBlockReaderKeepTracksParity(t *testing.T) {
	fixture := buildTwoTrackFixture(t)

	full, err := NewBlockReader(bytes.NewReader(fixture), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	var want []mkv.Block
	for _, b := range drain(t, full) {
		if b.TrackNumber == 2 {
			want = append(want, b)
		}
	}
	if len(want) == 0 {
		t.Fatal("fixture has no track-2 blocks")
	}

	filtered, err := NewBlockReader(bytes.NewReader(fixture), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	filtered.KeepTracks(2)
	got := drain(t, filtered)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filtered walk differs from full walk filtered:\n got %d blocks: %+v\nwant %d blocks: %+v",
			len(got), got, len(want), want)
	}
}

// The filtered walk must skip the heavy payloads' bytes, not read them: with
// ~9 MB of track-1 payload and a few hundred bytes of track 2, reading more
// than a third of the file means payloads are being dragged through the
// buffer (the defect that made the subtitle cue pass read whole files).
func TestBlockReaderKeepTracksBoundedIO(t *testing.T) {
	fixture := buildTwoTrackFixture(t)

	src := &countingSeeker{r: bytes.NewReader(fixture)}
	br, err := NewBlockReader(src, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	br.KeepTracks(2)
	got := drain(t, br)
	if len(got) == 0 {
		t.Fatal("filtered walk returned no blocks")
	}
	if limit := int64(len(fixture)) / 3; src.n > limit {
		t.Errorf("filtered walk read %d of %d bytes (%.0f%%) - non-kept payloads must be seeked past",
			src.n, len(fixture), 100*float64(src.n)/float64(len(fixture)))
	}
}

// A declared block size larger than what the file holds must surface as an
// error on the seek-skip path too, not as a silent clean EOF.
func TestBlockReaderKeepTracksTruncatedSkip(t *testing.T) {
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)
	// Track-1 block declaring 1 MiB of data, with only a fraction present.
	payload := []byte{0x81, 0x00, 0x00, 0x80}
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(payload))+1<<20)
	cluster.Write(payload)
	cluster.Write(make([]byte, 4096))

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(cluster.Len())+1<<20)
	seg.Write(cluster.Bytes())

	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&full, mkv.IDSegment, int64(seg.Len())+1<<20)
	full.Write(seg.Bytes())

	br, err := NewBlockReader(bytes.NewReader(full.Bytes()), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	br.KeepTracks(2)
	if _, err := br.Next(); err == nil || err == io.EOF {
		t.Errorf("truncated skip: err = %v, want a real error", err)
	}
}

// buildSmallFrameFixture interleaves ~7 KiB track-1 payloads (the measured
// median of a real 1080p x264 encode - SMALLER than any fixed seek threshold)
// with sparse track-2 cues. This is the regime the first KeepTracks cut
// missed: per-frame skips fell below the seek threshold and the walk degraded
// to a full read through a tiny buffer - slower than a plain sequential pass.
func buildSmallFrameFixture(t *testing.T) []byte {
	t.Helper()
	frame := make([]byte, 7*1024)
	for i := range frame {
		frame[i] = byte(i)
	}
	var seg bytes.Buffer
	for c := 0; c < 40; c++ {
		var cluster bytes.Buffer
		ts := uint64(c * 1000)
		ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, int64(ebml.UintLen(ts)))
		ebml.WriteUint(&cluster, ts, ebml.UintLen(ts))
		for b := 0; b < 25; b++ {
			writeSimpleBlock(&cluster, 1, int16(b*40), 0x80, frame)
		}
		writeSimpleBlock(&cluster, 2, 0, 0x80, []byte("cue"))
		ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(cluster.Len()))
		seg.Write(cluster.Bytes())
	}
	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&full, mkv.IDSegment, int64(seg.Len()))
	full.Write(seg.Bytes())
	return full.Bytes()
}

// When payloads are too small to seek over, the filtered walk cannot read
// less than the file - but it must then behave like a BULK sequential read:
// few large source reads, not one small read per block. Read-call count is
// the proxy for per-request cost (network filesystems pay a round trip per
// call): reading a ~7 MB fixture must take at most size/32KiB calls, and no
// byte may be read twice.
func TestBlockReaderKeepTracksSmallFramesBulkReads(t *testing.T) {
	fixture := buildSmallFrameFixture(t)

	src := &countingSeeker{r: bytes.NewReader(fixture)}
	br, err := NewBlockReader(src, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	br.KeepTracks(2)
	got := drain(t, br)
	if want := 40; len(got) != want {
		t.Fatalf("got %d track-2 blocks, want %d", len(got), want)
	}
	if maxCalls := int64(len(fixture))/(32<<10) + 8; src.calls > maxCalls {
		t.Errorf("small-frame walk issued %d source reads for %d bytes (max %d) - must read in bulk chunks, not per block",
			src.calls, len(fixture), maxCalls)
	}
	if limit := int64(len(fixture)) + int64(len(fixture))/10; src.n > limit {
		t.Errorf("small-frame walk read %d bytes for a %d-byte file - bytes are being re-read", src.n, len(fixture))
	}
}

// buildTimedClusterFixture builds numClusters known-size clusters (TS 0,
// 1000, 2000…), each holding track-1 blocks; track 2 appears only in the
// first cluster (the sparse-track case).
func buildTimedClusterFixture(t *testing.T, numClusters int) []byte {
	t.Helper()
	var seg bytes.Buffer
	for c := 0; c < numClusters; c++ {
		var cluster bytes.Buffer
		ts := uint64(c * 1000)
		ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, int64(ebml.UintLen(ts)))
		ebml.WriteUint(&cluster, ts, ebml.UintLen(ts))
		for b := 0; b < 3; b++ {
			data := append(make([]byte, 2<<10), byte(c), byte(b))
			writeSimpleBlock(&cluster, 1, int16(b*10), 0x80, data)
		}
		if c == 0 {
			writeSimpleBlock(&cluster, 2, 5, 0x80, []byte("piste2"))
		}
		ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(cluster.Len()))
		seg.Write(cluster.Bytes())
	}
	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&full, mkv.IDSegment, int64(seg.Len()))
	full.Write(seg.Bytes())
	return full.Bytes()
}

// StopBeforeClusterMs must stop the walk before delivering anything from the
// first cluster whose timestamp exceeds the limit; the limit can be raised on
// the same reader to continue, and ResumeOffset must allow a NEW reader to
// pick up exactly where the walk stopped - concatenation equals a full walk.
func TestBlockReaderStopBeforeClusterMs(t *testing.T) {
	fixture := buildTimedClusterFixture(t, 5)

	full, err := NewBlockReader(bytes.NewReader(fixture), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	want := drain(t, full)

	br, err := NewBlockReader(bytes.NewReader(fixture), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	br.StopBeforeClusterMs(1500)
	var got []mkv.Block
	for {
		b, err := br.Next()
		if errors.Is(err, ErrClusterLimit) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, b)
	}
	if len(got) != 7 { // cluster TS 0 (3 track-1 + 1 track-2) + cluster TS 1000 (3)
		t.Fatalf("stopped walk returned %d blocks, want 7 (clusters 0 and 1000)", len(got))
	}
	for _, b := range got {
		if b.Timecode >= 2000 {
			t.Errorf("block at %dms delivered past the 1500ms cluster limit", b.Timecode)
		}
	}

	// Raising the limit on the SAME reader continues into the held cluster.
	br.StopBeforeClusterMs(3500)
	for {
		b, err := br.Next()
		if errors.Is(err, ErrClusterLimit) {
			break
		}
		if err != nil {
			t.Fatalf("Next after raise: %v", err)
		}
		got = append(got, b)
	}

	// Resuming with a fresh reader at ResumeOffset yields the tail exactly.
	resume := br.ResumeOffset()
	br2, err := NewBlockReaderAt(bytes.NewReader(fixture), 1_000_000, resume)
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, drain(t, br2)...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stop+raise+resume walk differs from full walk:\n got %d blocks\nwant %d blocks", len(got), len(want))
	}
}

// On a sparse filtered track the limit is what bounds the walk: with track 2
// present only in the first cluster, a filtered walk with a limit must stop
// at the limit - NOT silently scan every remaining cluster hunting for a
// block that never comes.
func TestBlockReaderStopBoundsSparseFilteredWalk(t *testing.T) {
	fixture := buildTimedClusterFixture(t, 50)
	src := &countingSeeker{r: bytes.NewReader(fixture)}
	br, err := NewBlockReader(src, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	br.KeepTracks(2)
	br.StopBeforeClusterMs(1500)
	var got []mkv.Block
	for {
		b, err := br.Next()
		if errors.Is(err, ErrClusterLimit) {
			break
		}
		if err == io.EOF {
			t.Fatal("filtered walk ran to EOF - the cluster limit did not bound it")
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, b)
	}
	if len(got) != 1 || string(got[0].Data) != "piste2" {
		t.Fatalf("got %d blocks, want the single track-2 cue", len(got))
	}
	if limit := int64(len(fixture)) / 2; src.n > limit {
		t.Errorf("bounded filtered walk read %d of %d bytes - it should stop at the limit", src.n, len(fixture))
	}
}

// A block declaring an unknown size must be refused even when the track filter
// would drop it: the filter discards "size - consumed", a negative count is a
// no-op, and the walk would then resume INSIDE the payload and parse it as
// element headers - emitting fabricated blocks with no error at all.
func TestBlockReaderKeepTracksRejectsUnknownSizeBlock(t *testing.T) {
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)
	// Track-1 SimpleBlock with an unknown (all-ones VINT) size, followed by
	// bytes that would parse as plausible elements if the walk fell into them.
	cluster.Write([]byte{0xA3, 0xFF}) // SimpleBlock, unknown size
	cluster.Write([]byte{0x81})       // its track VINT: track 1
	// What follows is the block's PAYLOAD. It is shaped as a well-formed
	// SimpleBlock for track 2 so that a walk which wrongly resumes here reads
	// it as an element and hands back a block that exists nowhere in the file.
	cluster.Write(bytes.Repeat([]byte{0xA3, 0x84, 0x82, 0x00, 0x00, 0x80}, 8))

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(cluster.Len()))
	seg.Write(cluster.Bytes())

	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&full, mkv.IDSegment, int64(seg.Len()))
	full.Write(seg.Bytes())

	for _, tc := range []struct {
		name string
		keep bool
	}{{"unfiltered", false}, {"filtered", true}} {
		t.Run(tc.name, func(t *testing.T) {
			br, err := NewBlockReader(bytes.NewReader(full.Bytes()), 1_000_000)
			if err != nil {
				t.Fatal(err)
			}
			if tc.keep {
				br.KeepTracks(2) // track 1 is filtered out
			}
			b, err := br.Next()
			if err == nil {
				t.Fatalf("Next() returned block %+v with no error - it was fabricated from the payload",
					b)
			}
			if err == io.EOF {
				t.Fatal("Next() = io.EOF - an unknown-size block must be reported, not skipped")
			}
		})
	}
}
