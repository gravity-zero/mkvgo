package ops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// tailTagsMKV writes a file whose Tags (n SimpleTags of 200 characters) sit after the clusters, past what the
// head's reserve can hold, as in-place tag editors leave them.
func tailTagsMKV(t *testing.T, path string, n int) []mkv.Tag {
	f, err := os.Create(path)
	mustNil(t, err)
	defer f.Close()
	var simple []mkv.SimpleTag
	for i := range n {
		simple = append(simple, mkv.SimpleTag{Name: "NOTE_" + string(rune('A'+i%26)), Value: strings.Repeat("x", 200)})
	}
	tags := []mkv.Tag{{TargetType: "MOVIE", SimpleTags: simple}}
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}, []mkv.Track{videoTrack(1)}, 2000))
	for tc := int64(0); tc < 2000; tc += 1000 {
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: make([]byte, 500)}}))
	}
	mustNil(t, mw.WriteTagsElement(tags))
	mustNil(t, mw.Finalize())
	return tags
}

// TestEditInPlaceLeavesTailTagsItDoesNotChange pins that a title edit on a file whose Tags sit after the clusters,
// too large for the head, lands in place and leaves them where they are.
func TestEditInPlaceLeavesTailTagsItDoesNotChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.mkv")
	tailTagsMKV(t, path, 30)
	before, _ := os.Stat(path)
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "New title" }); err != nil {
		t.Fatalf("a title edit was refused: %v", err)
	}
	after, _ := os.Stat(path)
	c, err := reader.OpenMeta(context.Background(), path, reader.WithTags())
	mustNil(t, err)
	if c.Info.Title != "New title" || len(c.Tags) != 1 || len(c.Tags[0].SimpleTags) != 30 || after.Size() != before.Size() {
		t.Errorf("title %q, %d tags, size %d -> %d; want the title, the 30 notes reached through the SeekHead, the same size", c.Info.Title, len(c.Tags), before.Size(), after.Size())
	}
}

// TestEditInPlaceRewritesTailTagsWhereTheyAre pins that tags edited on such a file, too large for the head but no
// larger than their old element, are rewritten in its place.
func TestEditInPlaceRewritesTailTagsWhereTheyAre(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.mkv")
	tailTagsMKV(t, path, 30)
	err := EditInPlace(context.Background(), path, func(c *mkv.Container) {
		c.Tags[0].SimpleTags = c.Tags[0].SimpleTags[:29]
		c.Tags[0].SimpleTags[0].Value = "edited"
	})
	if err != nil {
		t.Fatalf("a tag edit that fits the old Tags element was refused: %v", err)
	}
	c, err := reader.Open(context.Background(), path)
	mustNil(t, err)
	if len(c.Tags) != 1 || len(c.Tags[0].SimpleTags) != 29 || c.Tags[0].SimpleTags[0].Value != "edited" {
		t.Errorf("tags after the edit: %+v", c.Tags)
	}
}

// TestEditInPlaceRefusalLeavesTheFileAsItWas pins that an edit with no room is refused as ErrNoRoomInPlace, its
// message unchanged, before a single byte of the file is written.
func TestEditInPlaceRefusalLeavesTheFileAsItWas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.mkv")
	tailTagsMKV(t, path, 30)
	before, err := os.ReadFile(path)
	mustNil(t, err)
	err = EditInPlace(context.Background(), path, func(c *mkv.Container) {
		c.Info.Title = "New title"
		c.Tags[0].SimpleTags = append(c.Tags[0].SimpleTags, mkv.SimpleTag{Name: "MORE", Value: strings.Repeat("y", 400)})
	})
	if !errors.Is(err, ErrNoRoomInPlace) || !strings.HasPrefix(err.Error(), "new metadata (") || !strings.HasSuffix(err.Error(), " - use full rewrite instead") {
		t.Fatalf("err = %v, want ErrNoRoomInPlace with the usual message", err)
	}
	after, err := os.ReadFile(path)
	mustNil(t, err)
	if !bytes.Equal(before, after) {
		t.Errorf("a refused edit changed the file")
	}
}

// TestEditInPlaceOnAnUnknownSizeSegment pins that a Segment of unknown size (a live recording) is edited in place,
// its clusters untouched.
func TestEditInPlaceOnAnUnknownSizeSegment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.mkv")
	tailTagsMKV(t, path, 2)
	data, err := os.ReadFile(path)
	mustNil(t, err)
	seg := bytes.Index(data, []byte{0x18, 0x53, 0x80, 0x67})
	if seg < 0 || data[seg+4] != 0x01 {
		t.Fatalf("no 8-byte Segment size to mark unknown")
	}
	copy(data[seg+4:], []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	mustNil(t, os.WriteFile(path, data, 0o644))
	blocks := len(collectAllBlocks(t, path))
	mustNil(t, EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "Live" }))
	c, err := reader.Open(context.Background(), path)
	mustNil(t, err)
	if c.Info.Title != "Live" || len(c.Tags) != 1 || len(collectAllBlocks(t, path)) != blocks {
		t.Errorf("title %q, %d tags, %d blocks of %d", c.Info.Title, len(c.Tags), len(collectAllBlocks(t, path)), blocks)
	}
}

// TestEditInPlaceWithoutSeekHead pins that a file with no SeekHead, its Tags after the clusters, is edited in place
// without a second copy of the tags.
func TestEditInPlaceWithoutSeekHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "noseek.mkv")
	tailTagsMKV(t, path, 2)
	data, err := os.ReadFile(path)
	mustNil(t, err)
	sh := bytes.Index(data, []byte{0x11, 0x4D, 0x9B, 0x74})
	if sh < 0 {
		t.Fatal("no SeekHead to remove")
	}
	r := bytes.NewReader(data[sh:])
	h, hdrLen, err := ebml.ReadElementHeader(r)
	mustNil(t, err)
	var void bytes.Buffer
	mustNil(t, writer.WriteVoid(&void, hdrLen+int(h.Size)))
	copy(data[sh:], void.Bytes())
	mustNil(t, os.WriteFile(path, data, 0o644))
	mustNil(t, EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "No index" }))
	c, err := reader.Open(context.Background(), path)
	mustNil(t, err)
	n := 0
	for _, tag := range c.Tags {
		n += len(tag.SimpleTags)
	}
	if c.Info.Title != "No index" || n != 2 {
		t.Errorf("title %q, %d simple tags; want the title and the 2 notes once", c.Info.Title, n)
	}
}

// TestEditInPlaceWithChaptersAfterTheClusters pins that chapters a muxer left after the clusters (as many
// muxers write them) stay there, once, when an edit does not touch them, and are not written a second time in the head.
func TestEditInPlaceWithChaptersAfterTheClusters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tailchap.mkv")
	f, err := os.Create(path)
	mustNil(t, err)
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}, []mkv.Track{videoTrack(1)}, 2000))
	for tc := int64(0); tc < 2000; tc += 1000 {
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: make([]byte, 100)}}))
	}
	mw.ChaptersPos = mw.RelPos()
	mustNil(t, writer.WriteEditions(f, []mkv.Chapter{{ID: 1, Title: "One", StartMs: 0}, {ID: 2, Title: "Two", StartMs: 1000}}, nil))
	mustNil(t, mw.Finalize())
	f.Close()
	before, _ := os.Stat(path)
	mustNil(t, EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "t" }))
	c, err := reader.Open(context.Background(), path)
	mustNil(t, err)
	after, _ := os.Stat(path)
	if c.Info.Title != "t" || len(c.Chapters) != 2 || after.Size() != before.Size() {
		t.Errorf("title %q, %d chapters, size %d -> %d; want the title, the 2 chapters once, the same size", c.Info.Title, len(c.Chapters), before.Size(), after.Size())
	}
}

// TestEditInPlaceWithWideSizeFields pins that a head whose last element states its size in more bytes than it needs
// (common muxers write 8) is rewritten to its real end: the bytes before the first Cluster are all accounted for,
// none of the old element left between the new head and the Cluster.
func TestEditInPlaceWithWideSizeFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wide.mkv")
	var tracks bytes.Buffer
	mustNil(t, writer.WriteTracks(&tracks, []mkv.Track{videoTrack(1)}))
	h, hdrLen, err := ebml.ReadElementHeader(bytes.NewReader(tracks.Bytes()))
	mustNil(t, err)
	var wide bytes.Buffer
	wide.Write([]byte{0x16, 0x54, 0xAE, 0x6B})
	_, err = ebml.WriteDataSizeWidth(&wide, h.Size, 8)
	mustNil(t, err)
	wide.Write(tracks.Bytes()[hdrLen:])
	var info bytes.Buffer
	mustNil(t, writer.WriteSegmentInfo(&info, &mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}, 1000))
	var cluster bytes.Buffer
	_, err = writer.WriteClusterOffsets(&cluster, 0, 1_000_000, []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: make([]byte, 50)}})
	mustNil(t, err)
	var seg bytes.Buffer
	seg.Write(info.Bytes())
	mustNil(t, writer.WriteVoid(&seg, 64)) // room for the title, ahead of the Tracks so they stay last
	seg.Write(wide.Bytes())
	seg.Write(cluster.Bytes())
	var file bytes.Buffer
	file.Write([]byte{0x1A, 0x45, 0xDF, 0xA3, 0xA3, 0x42, 0x86, 0x81, 0x01, 0x42, 0xF7, 0x81, 0x01, 0x42, 0xF2, 0x81, 0x04, 0x42, 0xF3, 0x81, 0x08, 0x42, 0x82, 0x88, 'm', 'a', 't', 'r', 'o', 's', 'k', 'a', 0x42, 0x87, 0x81, 0x04, 0x42, 0x85, 0x81, 0x02})
	file.Write([]byte{0x18, 0x53, 0x80, 0x67})
	_, err = ebml.WriteDataSizeWidth(&file, int64(seg.Len()), 8)
	mustNil(t, err)
	file.Write(seg.Bytes())
	mustNil(t, os.WriteFile(path, file.Bytes(), 0o644))
	mustNil(t, EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "x" }))
	data, err := os.ReadFile(path)
	mustNil(t, err)
	clusterAt := bytes.Index(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	r := bytes.NewReader(data[bytes.Index(data, []byte{0x18, 0x53, 0x80, 0x67})+12:])
	pos := int64(len(data) - r.Len())
	for pos < int64(clusterAt) {
		eh, n, err := ebml.ReadElementHeader(r)
		if err != nil {
			t.Fatalf("head unreadable at %d: %v", pos, err)
		}
		pos += int64(n) + eh.Size
		if _, err := r.Seek(eh.Size, io.SeekCurrent); err != nil {
			t.Fatal(err)
		}
	}
	if pos != int64(clusterAt) {
		t.Errorf("the head's elements end at %d, the Cluster starts at %d: bytes of the old head left between", pos, clusterAt)
	}
}

// TestEditInPlaceWithTagsInTheHeadAndAfterTheClusters pins a title edit on a file holding Tags in its head and more
// after its clusters (as common muxers leave them): both stay where they are, as they were, and the edit lands in place.
func TestEditInPlaceWithTagsInTheHeadAndAfterTheClusters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "both.mkv")
	f, err := os.Create(path)
	mustNil(t, err)
	head := []mkv.Tag{{TargetType: "MOVIE", SimpleTags: []mkv.SimpleTag{{Name: "COMMENT", Value: "head"}}}}
	var tail []mkv.SimpleTag
	for i := range 20 {
		tail = append(tail, mkv.SimpleTag{Name: "NOTE_" + string(rune('A'+i)), Value: strings.Repeat("x", 100)})
	}
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}, Tags: head}, []mkv.Track{videoTrack(1)}, 2000))
	for tc := int64(0); tc < 2000; tc += 1000 {
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: make([]byte, 100)}}))
	}
	mw.TagsPos = mw.RelPos() // the SeekHead points at the Tags after the clusters, not at those in the head
	mustNil(t, mw.WriteTagsElement([]mkv.Tag{{TargetType: "ALBUM", SimpleTags: tail}}))
	mustNil(t, mw.Finalize())
	f.Close()
	before, _ := os.Stat(path)
	mustNil(t, EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "t" }))
	c, err := reader.Open(context.Background(), path)
	mustNil(t, err)
	after, _ := os.Stat(path)
	n := 0
	for _, tag := range c.Tags {
		n += len(tag.SimpleTags)
	}
	if c.Info.Title != "t" || n != 21 || after.Size() != before.Size() {
		t.Errorf("title %q, %d simple tags, size %d -> %d; want the title, the 21 tags once, the same size", c.Info.Title, n, before.Size(), after.Size())
	}
}
