package ops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// tightHeadMKV writes a file whose head holds a SeekHead, the Info, the Tracks and the Tags with no Void between
// them and the first Cluster, as several muxers leave it: no edit that grows the metadata fits the head.
func tightHeadMKV(t *testing.T, path string, tags []mkv.Tag) {
	t.Helper()
	data, err := tightHeadBytes(tags)
	mustNil(t, err)
	mustNil(t, os.WriteFile(path, data, 0o644))
}

func tightHeadBytes(tags []mkv.Tag) ([]byte, error) {
	var info, tracks, tagsEl, clusters bytes.Buffer
	if err := writer.WriteSegmentInfo(&info, &mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}, 2000); err != nil {
		return nil, err
	}
	if err := writer.WriteTracks(&tracks, []mkv.Track{videoTrack(1)}); err != nil {
		return nil, err
	}
	if len(tags) > 0 {
		if err := writer.WriteTags(&tagsEl, tags); err != nil {
			return nil, err
		}
	}
	for tc := int64(0); tc < 2000; tc += 1000 {
		if _, err := writer.WriteClusterOffsets(&clusters, tc, 1_000_000, []mkv.Block{{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: make([]byte, 300)}}); err != nil {
			return nil, err
		}
	}
	var sh bytes.Buffer
	for range 3 { // the SeekHead's own size moves the positions it records: a few rounds settle it
		base := int64(sh.Len())
		entries := []writer.SeekEntry{{ID: mkv.IDInfo, Pos: base}, {ID: mkv.IDTracks, Pos: base + int64(info.Len())}}
		if tagsEl.Len() > 0 {
			entries = append(entries, writer.SeekEntry{ID: mkv.IDTags, Pos: base + int64(info.Len()+tracks.Len())})
		}
		sh.Reset()
		if err := writer.WriteSeekHead(&sh, entries); err != nil {
			return nil, err
		}
	}
	var seg bytes.Buffer
	for _, b := range [][]byte{sh.Bytes(), info.Bytes(), tracks.Bytes(), tagsEl.Bytes(), clusters.Bytes()} {
		seg.Write(b)
	}
	var file bytes.Buffer
	file.Write([]byte{0x1A, 0x45, 0xDF, 0xA3, 0xA3, 0x42, 0x86, 0x81, 0x01, 0x42, 0xF7, 0x81, 0x01, 0x42, 0xF2, 0x81, 0x04, 0x42, 0xF3, 0x81, 0x08, 0x42, 0x82, 0x88, 'm', 'a', 't', 'r', 'o', 's', 'k', 'a', 0x42, 0x87, 0x81, 0x04, 0x42, 0x85, 0x81, 0x02})
	file.Write([]byte{0x18, 0x53, 0x80, 0x67})
	if _, err := ebml.WriteDataSizeWidth(&file, int64(seg.Len()), 8); err != nil {
		return nil, err
	}
	file.Write(seg.Bytes())
	return file.Bytes(), nil
}

// appendTrailingVoid puts bytes after the Segment, which an element moved to its end would overwrite.
func appendTrailingVoid(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	mustNil(t, err)
	defer f.Close()
	mustNil(t, writer.WriteVoid(f, 16))
}

func movieTags(value string) []mkv.Tag {
	return []mkv.Tag{{TargetType: "MOVIE", SimpleTags: []mkv.SimpleTag{{Name: "TITLE", Value: value}, {Name: "COMMENT", Value: "kept"}}}}
}

// segmentSize reads the Segment's declared size, -1 when unknown.
func segmentSize(t *testing.T, data []byte) (int64, int) {
	t.Helper()
	at := bytes.Index(data, []byte{0x18, 0x53, 0x80, 0x67})
	h, hdrLen, err := ebml.ReadElementHeader(bytes.NewReader(data[at:]))
	mustNil(t, err)
	return h.Size, at + hdrLen
}

// TestEditInPlaceMovesTagsToTheSegmentEnd pins the RFC 9559 6.8 way out of a full head: the Tags are voided where
// they were and written again at the end of the Segment, which grows by them, so the edited Info fits the head.
func TestEditInPlaceMovesTagsToTheSegmentEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tight.mkv")
	tightHeadMKV(t, path, movieTags("A title long enough to free some room"))
	blocks := len(collectAllBlocks(t, path))
	err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "A title the head had no room for" })
	if err != nil {
		t.Fatalf("an Info edit on a full head with Tags to move was refused: %v", err)
	}
	data, err := os.ReadFile(path)
	mustNil(t, err)
	size, dataStart := segmentSize(t, data)
	if size != int64(len(data)-dataStart) {
		t.Errorf("Segment declares %d bytes, holds %d: the moved Tags sit outside it", size, len(data)-dataStart)
	}
	c, err := reader.Open(context.Background(), path)
	mustNil(t, err)
	n := 0
	for _, tag := range c.Tags {
		n += len(tag.SimpleTags)
	}
	if c.Info.Title != "A title the head had no room for" || n != 2 || len(c.Tracks) != 1 || len(collectAllBlocks(t, path)) != blocks {
		t.Errorf("title %q, %d simple tags, %d tracks, %d blocks of %d; want the title, the 2 tags once, the track, every block", c.Info.Title, n, len(c.Tracks), len(collectAllBlocks(t, path)), blocks)
	}
	meta, err := reader.OpenMeta(context.Background(), path, reader.WithTags())
	mustNil(t, err)
	if len(meta.Tags) != 1 {
		t.Errorf("head-only read found %d Tags, want the moved ones through the SeekHead", len(meta.Tags))
	}
}

// TestEditInPlaceMovesTheEditedInfoWhenNothingElseCan pins a full head with no Tags (a WebM, say): the edited Info
// itself goes to the end of the Segment, which RFC 9559 6.1 allows once the head SeekHead points at it.
func TestEditInPlaceMovesTheEditedInfoWhenNothingElseCan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tight.webm")
	tightHeadMKV(t, path, nil)
	blocks := len(collectAllBlocks(t, path))
	title := strings.Repeat("long title ", 6)
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = title }); err != nil {
		t.Fatalf("an Info edit on a full head was refused: %v", err)
	}
	meta, err := reader.OpenMeta(context.Background(), path)
	mustNil(t, err)
	if meta.Info.Title != title || len(meta.Tracks) != 1 || meta.Info.TimecodeScale != 1_000_000 || meta.DurationMs != 2000 {
		t.Errorf("head-only read: title %q, %d tracks, scale %d, duration %d", meta.Info.Title, len(meta.Tracks), meta.Info.TimecodeScale, meta.DurationMs)
	}
	if n := len(collectAllBlocks(t, path)); n != blocks {
		t.Errorf("%d blocks of %d after the edit", n, blocks)
	}
}

// TestEditInPlaceAppendsToAnUnknownSizeSegment pins that a Segment of unknown size takes the moved element at the
// end of the file, its size left unknown.
func TestEditInPlaceAppendsToAnUnknownSizeSegment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.mkv")
	tightHeadMKV(t, path, movieTags("short"))
	data, err := os.ReadFile(path)
	mustNil(t, err)
	_, dataStart := segmentSize(t, data)
	copy(data[dataStart-8:], []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	mustNil(t, os.WriteFile(path, data, 0o644))
	mustNil(t, EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "A title the head had no room for" }))
	c, err := reader.Open(context.Background(), path)
	mustNil(t, err)
	if c.Info.Title != "A title the head had no room for" || len(c.Tags) != 1 {
		t.Errorf("title %q, %d tags", c.Info.Title, len(c.Tags))
	}
	after, err := os.ReadFile(path)
	mustNil(t, err)
	if size, _ := segmentSize(t, after); size != -1 {
		t.Errorf("Segment size %d, want it left unknown", size)
	}
}

// TestEditInPlaceRefusesToAppendPastTheSegment pins that bytes after the Segment (a second one, trailing data) leave
// nowhere to move an element to: a full head is refused as before, the file untouched.
func TestEditInPlaceRefusesToAppendPastTheSegment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trailing.mkv")
	tightHeadMKV(t, path, movieTags("short"))
	appendTrailingVoid(t, path)
	data, err := os.ReadFile(path)
	mustNil(t, err)
	err = EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "A title the head had no room for" })
	if !errors.Is(err, ErrNoRoomInPlace) {
		t.Fatalf("err = %v, want ErrNoRoomInPlace", err)
	}
	after, err := os.ReadFile(path)
	mustNil(t, err)
	if !bytes.Equal(data, after) {
		t.Errorf("a refused edit changed the file")
	}
}
