package reader

import (
	"bytes"
	"context"
	"math/rand"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// mediaCluster builds a Cluster holding one SimpleBlock of n pseudo-random bytes, like coded media.
func mediaCluster(rng *rand.Rand, n int) []byte {
	block := make([]byte, 4+n)
	block[0], block[3] = 0x81, 0x80 // track 1, keyframe
	rng.Read(block[4:])
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, mkv.IDSimpleBlock, int64(len(block)))
	b.Write(block)
	return masterElem(mkv.IDCluster, uintElem(mkv.IDTimestamp, 0, 1), b.Bytes())
}

// tailReadBytes reads meta through an ioCounter and returns the container and
// the bytes read at or after from (the tail scan's reads; the head is before).
func tailReadBytes(t *testing.T, data []byte, from int64, opts ...ReadOption) (*mkv.Container, int64) {
	t.Helper()
	cnt := &tailCounter{rs: bytes.NewReader(data), from: from}
	c, err := ReadMeta(context.Background(), cnt, "x.mkv", opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c, cnt.tail
}

// tailCounter counts the bytes read at or after offset from.
type tailCounter struct {
	rs   *bytes.Reader
	from int64
	tail int64
}

func (c *tailCounter) Read(p []byte) (int, error) {
	pos, _ := c.rs.Seek(0, 1)
	n, err := c.rs.Read(p)
	if end := pos + int64(n); end > c.from {
		c.tail += end - max(pos, c.from)
	}
	return n, err
}

func (c *tailCounter) Seek(off int64, whence int) (int64, error) { return c.rs.Seek(off, whence) }

// TestTailScanReadsWhatTheTailHolds: with no SeekHead, the read back from EOF
// for the tags, chapters and Cues starts small and doubles while it lands
// inside them, so a file that ends on a cluster costs 64 KiB, and a tail
// larger than that is still read whole.
func TestTailScanReadsWhatTheTailHolds(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var clusters []byte
	for i := 0; i < 12; i++ {
		clusters = append(clusters, mediaCluster(rng, 200<<10)...)
	}
	head := segmentMKV(infoElem(), tracksElem())
	opts := []ReadOption{WithCues(), WithTags(), WithChapters(), WithAttachments(), WithoutAttachmentData()}

	plain := segmentMKV(infoElem(), tracksElem(), clusters)
	c, tail := tailReadBytes(t, plain, int64(len(head)+len(clusters)/2), opts...)
	if len(c.Cues) != 0 || len(c.Tags) != 0 || len(c.Chapters) != 0 {
		t.Errorf("a file ending on a cluster: found %d cues, %d tags, %d chapters, want none", len(c.Cues), len(c.Tags), len(c.Chapters))
	}
	if tail > 64<<10 {
		t.Errorf("a file ending on a cluster: the tail scan read %d bytes, want at most 64 KiB", tail)
	}

	cues := cuesElem(12000) // about 200 KiB of cue points: larger than the first window
	full := segmentMKV(infoElem(), tracksElem(), clusters, cues, tagsElem("ENCODER", "mkvgo-test"), chaptersElem(), attachmentsElem())
	c, tail = tailReadBytes(t, full, int64(len(head)+len(clusters)/2), opts...)
	if len(c.Cues) != 12000 || len(c.Tags) != 1 || len(c.Chapters) != 1 || len(c.Attachments) != 1 {
		t.Errorf("a %d-byte tail: found %d cues, %d tags, %d chapters, %d attachments; want 12000, 1, 1, 1",
			len(cues), len(c.Cues), len(c.Tags), len(c.Chapters), len(c.Attachments))
	}
	if tail > 512<<10 {
		t.Errorf("a %d-byte tail: the tail scan read %d bytes, want at most 512 KiB", len(cues), tail)
	}
	t.Logf("tail of %d bytes: read %d", len(cues), tail)
}

// TestTailScanRunWithTrailingSeekHeads: a muxer may close the file with the
// Cues, the Tags and a SeekHead or two after them; the tail run takes them in.
func TestTailScanRunWithTrailingSeekHeads(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	var clusters []byte
	for i := 0; i < 4; i++ {
		clusters = append(clusters, mediaCluster(rng, 200<<10)...)
	}
	sh := seekHeadElem(seekEntry(mkv.IDCues, 1), seekEntry(mkv.IDTags, 2))
	data := segmentMKV(infoElem(), tracksElem(), clusters, cuesElem(5000), tagsElem("BPS", "24494738"), sh, sh)
	c, _ := tailReadBytes(t, data, 0, WithCues(), WithTags())
	if len(c.Cues) != 5000 || len(c.Tags) != 1 {
		t.Errorf("found %d cues and %d tags, want 5000 and 1", len(c.Cues), len(c.Tags))
	}
}
