package ops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// tailTagsMKV writes a file whose Tags (n SimpleTags of 200 characters) sit after the clusters, past what the
// head's reserve can hold, as mkvpropedit leaves them.
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
