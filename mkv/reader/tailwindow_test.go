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

// TestMetaAttachmentOffsetsPastExactReads: past 256 KiB of skipped payload a
// metadata read goes through exact windows; each attachment still records
// where its payload is, and the elements after them are still read.
func TestMetaAttachmentOffsetsPastExactReads(t *testing.T) {
	file := func(name string, n int) []byte {
		var data bytes.Buffer
		ebml.WriteElementHeader(&data, mkv.IDFileData, int64(n))
		data.Write(bytes.Repeat([]byte{byte(n)}, n))
		return masterElem(mkv.IDAttachedFile, strElem(mkv.IDFileName, name), strElem(mkv.IDFileMimeType, "font/ttf"),
			data.Bytes(), uintElem(mkv.IDFileUID, uint64(n), 8))
	}
	atts := masterElem(mkv.IDAttachments, file("a.ttf", 300<<10), file("b.ttf", 200<<10), file("c.ttf", 5))
	data := segmentMKV(infoElem(), tracksElem(), atts, tagsElem("TITLE", "t"), clusterElem())
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv", WithAttachments(), WithoutAttachmentData(), WithTags())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Attachments) != 3 || len(c.Tags) != 1 {
		t.Fatalf("%d attachments and %d tags, want 3 and 1", len(c.Attachments), len(c.Tags))
	}
	for _, a := range c.Attachments {
		if got := data[a.DataOffset : a.DataOffset+a.Size]; !bytes.Equal(got, bytes.Repeat([]byte{byte(a.Size)}, int(a.Size))) || a.ID != uint64(a.Size) {
			t.Errorf("%s: DataOffset %d, size %d, UID %d do not name its payload", a.Name, a.DataOffset, a.Size, a.ID)
		}
	}
}

// TestMetaFollowsSecondarySeekHead: a head SeekHead that only points at a
// second SeekHead at the end of the file; the elements that one indexes are
// read at their offsets, with no read back from EOF.
func TestMetaFollowsSecondarySeekHead(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	var clusters []byte
	for i := 0; i < 4; i++ {
		clusters = append(clusters, mediaCluster(rng, 200<<10)...)
	}
	info, tracks := infoElem(), tracksElem()
	cues, tags, chapters := cuesElem(12000), tagsElem("BPS", "24494738"), chaptersElem()
	headSH := func(pos uint64) []byte { return seekHeadElem(seekEntry(mkv.IDSeekHead, pos)) }
	cuesPos := uint64(len(headSH(0)) + len(info) + len(tracks) + len(clusters))
	tagsPos := cuesPos + uint64(len(cues))
	chaptersPos := tagsPos + uint64(len(tags))
	tailSH := seekHeadElem(seekEntry(mkv.IDCues, cuesPos), seekEntry(mkv.IDTags, tagsPos), seekEntry(mkv.IDChapters, chaptersPos))
	tailLen := len(cues) + len(tags) + len(chapters) + len(tailSH)
	data := segmentMKV(headSH(chaptersPos+uint64(len(chapters))), info, tracks, clusters, cues, tags, chapters, tailSH)

	c, tail := tailReadBytes(t, data, int64(len(data)-tailLen), WithCues(), WithTags(), WithChapters(), WithAttachments(), WithoutAttachmentData())
	if len(c.Cues) != 12000 || len(c.Tags) != 1 || len(c.Chapters) != 1 {
		t.Errorf("found %d cues, %d tags, %d chapters; want 12000, 1, 1", len(c.Cues), len(c.Tags), len(c.Chapters))
	}
	if tail > int64(tailLen)+4<<10 {
		t.Errorf("read %d bytes of a %d-byte tail, want the indexed elements only", tail, tailLen)
	}
}

// TestReadBacksUpToElementsBeforeTailSeekHead: the head SeekHead points only at
// a second SeekHead written after the Cues and Tags; the full read, jumping to
// it past the clusters, goes back for the elements it indexes.
func TestReadBacksUpToElementsBeforeTailSeekHead(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	var clusters []byte
	for i := 0; i < 4; i++ {
		clusters = append(clusters, mediaCluster(rng, 200<<10)...)
	}
	info, tracks := infoElem(), tracksElem()
	cues, tags := cuesElem(50), tagsElem("BPS", "24494738")
	headSH := func(pos uint64) []byte { return seekHeadElem(seekEntry(mkv.IDSeekHead, pos)) }
	cuesPos := uint64(len(headSH(0)) + len(info) + len(tracks) + len(clusters))
	tailSH := seekHeadElem(seekEntry(mkv.IDCues, cuesPos), seekEntry(mkv.IDTags, cuesPos+uint64(len(cues))))
	data := segmentMKV(headSH(cuesPos+uint64(len(cues)+len(tags))), info, tracks, clusters, cues, tags, tailSH)
	c, err := Read(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Cues) != 50 || len(c.Tags) != 1 || len(c.Keyframes) == 0 {
		t.Errorf("found %d cues, %d tags, %d keyframes; want 50, 1 and the keyframes", len(c.Cues), len(c.Tags), len(c.Keyframes))
	}
}

// TestReadBacksUpOnceWithoutRereading: when the jump past the clusters landed on
// the Cues and a later SeekHead indexes Tags before them, the read goes back for
// the Tags and does not parse the Cues it already walked a second time.
func TestReadBacksUpOnceWithoutRereading(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	var clusters []byte
	for i := 0; i < 4; i++ {
		clusters = append(clusters, mediaCluster(rng, 200<<10)...)
	}
	info, tracks := infoElem(), tracksElem()
	tags, cues := tagsElem("BPS", "24494738"), cuesElem(50)
	headSH := func(cuesPos, shPos uint64) []byte {
		return seekHeadElem(seekEntry(mkv.IDCues, cuesPos), seekEntry(mkv.IDSeekHead, shPos))
	}
	tagsPos := uint64(len(headSH(0, 0)) + len(info) + len(tracks) + len(clusters))
	cuesPos := tagsPos + uint64(len(tags))
	tailSH := seekHeadElem(seekEntry(mkv.IDTags, tagsPos), seekEntry(mkv.IDCues, cuesPos))
	data := segmentMKV(headSH(cuesPos, cuesPos+uint64(len(cues))), info, tracks, clusters, tags, cues, tailSH)
	c, err := Read(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Cues) != 50 || len(c.Tags) != 1 {
		t.Errorf("found %d cues and %d tags, want 50 and 1", len(c.Cues), len(c.Tags))
	}
}

// readCounter counts the Read calls a source receives.
type readCounter struct {
	*bytes.Reader
	calls int
}

func (r *readCounter) Read(p []byte) (int, error) { r.calls++; return r.Reader.Read(p) }

// TestReadIgnoresClustersATailSeekHeadIndexes: a SeekHead at the end of the
// file that indexes every Cluster (as older muxers wrote it) does not send the
// full read back to the second cluster: it reads the tail, in a few calls.
func TestReadIgnoresClustersATailSeekHeadIndexes(t *testing.T) {
	rng := rand.New(rand.NewSource(6))
	info, tracks := infoElem(), tracksElem()
	headSH := func(cues, sh uint64) []byte {
		return seekHeadElem(seekEntry(mkv.IDCues, cues), seekEntry(mkv.IDSeekHead, sh))
	}
	start := uint64(len(headSH(0, 0)) + len(info) + len(tracks))
	var clusters []byte
	var entries [][]byte
	for i := 0; i < 3000; i++ {
		entries = append(entries, seekEntry(mkv.IDCluster, start+uint64(len(clusters))))
		clusters = append(clusters, mediaCluster(rng, 4<<10)...)
	}
	cues, tags := cuesElem(50), tagsElem("BPS", "24494738")
	cuesPos := start + uint64(len(clusters))
	tailSH := seekHeadElem(append(entries, seekEntry(mkv.IDCues, cuesPos), seekEntry(mkv.IDTags, cuesPos+uint64(len(cues))))...)
	data := segmentMKV(headSH(cuesPos, cuesPos+uint64(len(cues)+len(tags))), info, tracks, clusters, cues, tags, tailSH)
	src := &readCounter{Reader: bytes.NewReader(data)}
	c, err := Read(context.Background(), src, "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Cues) != 50 || len(c.Tags) != 1 {
		t.Errorf("found %d cues and %d tags, want 50 and 1", len(c.Cues), len(c.Tags))
	}
	if src.calls > 40 {
		t.Errorf("%d read calls for a %d-byte file, want at most 40: the read walked the clusters", src.calls, len(data))
	}
	t.Logf("%d read calls", src.calls)
}

// editionElem is one EditionEntry with n chapters starting at base seconds; def sets EditionFlagDefault.
func editionElem(uid uint64, def bool, base, n int) []byte {
	kids := [][]byte{uintElem(mkv.IDEditionUID, uid, 8)}
	if def {
		kids = append(kids, uintElem(mkv.IDEditionFlagDefault, 1, 1))
	}
	for i := 0; i < n; i++ {
		kids = append(kids, masterElem(mkv.IDChapterAtom,
			uintElem(mkv.IDChapterUID, uid*100+uint64(i), 8),
			uintElem(mkv.IDChapterTimeStart, uint64(base+i)*1_000_000_000, 8)))
	}
	return masterElem(mkv.IDEditionEntry, kids...)
}

// TestChapterEditions: the chapters are the default edition's (EditionFlagDefault,
// else the first), and the other editions come apart, never mixed into them.
func TestChapterEditions(t *testing.T) {
	for name, c := range map[string]struct {
		chapters      []byte
		want, wantOne int
	}{
		"second edition default": {masterElem(mkv.IDChapters, editionElem(1, false, 0, 2), editionElem(2, true, 100, 3)), 3, 2},
		"no default edition":     {masterElem(mkv.IDChapters, editionElem(1, false, 0, 2), editionElem(2, false, 100, 3)), 2, 3},
	} {
		data := segmentMKV(infoElem(), tracksElem(), c.chapters, clusterElem())
		for how, read := range map[string]func() (*mkv.Container, error){
			"Read": func() (*mkv.Container, error) { return Read(context.Background(), bytes.NewReader(data), "x.mkv") },
			"ReadMeta": func() (*mkv.Container, error) {
				return ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv", WithChapters())
			},
		} {
			got, err := read()
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Chapters) != c.want || len(got.OtherEditions) != 1 || len(got.OtherEditions[0].Chapters) != c.wantOne {
				t.Errorf("%s %s: %d chapters and %d other editions, want %d and 1 of %d", name, how, len(got.Chapters), len(got.OtherEditions), c.want, c.wantOne)
			}
		}
	}
}
