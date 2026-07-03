package reader

import (
	"bytes"
	"io"
	"reflect"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// countingSeeker counts the bytes and the calls actually read from the
// source — the probes that prove a filtered walk skips payloads instead of
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
// payloads — comfortably above the seek threshold: SimpleBlocks and a
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
// for that track — same order, timecodes, keyframes, durations, data —
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
		t.Errorf("filtered walk read %d of %d bytes (%.0f%%) — non-kept payloads must be seeked past",
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
// median of a real 1080p x264 encode — SMALLER than any fixed seek threshold)
// with sparse track-2 cues. This is the regime the first KeepTracks cut
// missed: per-frame skips fell below the seek threshold and the walk degraded
// to a full read through a tiny buffer — slower than a plain sequential pass.
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
// less than the file — but it must then behave like a BULK sequential read:
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
		t.Errorf("small-frame walk issued %d source reads for %d bytes (max %d) — must read in bulk chunks, not per block",
			src.calls, len(fixture), maxCalls)
	}
	if limit := int64(len(fixture)) + int64(len(fixture))/10; src.n > limit {
		t.Errorf("small-frame walk read %d bytes for a %d-byte file — bytes are being re-read", src.n, len(fixture))
	}
}
