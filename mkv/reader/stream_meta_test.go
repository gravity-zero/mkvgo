package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

func elemU(buf *bytes.Buffer, id uint32, v uint64) {
	ebml.WriteElementHeader(buf, id, int64(ebml.UintLen(v)))
	ebml.WriteUint(buf, v, ebml.UintLen(v))
}

func elemS(buf *bytes.Buffer, id uint32, s string) {
	ebml.WriteElementHeader(buf, id, int64(len(s)))
	ebml.WriteString(buf, s)
}

func elemB(buf *bytes.Buffer, id uint32, b []byte) {
	ebml.WriteElementHeader(buf, id, int64(len(b)))
	buf.Write(b)
}

func elemMaster(buf *bytes.Buffer, id uint32, body []byte) {
	ebml.WriteElementHeader(buf, id, int64(len(body)))
	buf.Write(body)
}

// TestReadStreamColourAttachmentsTags exercises the streaming parser's
// Colour (inside Video), Attachments and Tags paths from a non-seekable reader.
func TestReadStreamColourAttachmentsTags(t *testing.T) {
	// Video > Colour
	var colour bytes.Buffer
	elemU(&colour, mkv.IDColourMatrix, 9)
	elemU(&colour, mkv.IDColourTransfer, 16)
	elemU(&colour, mkv.IDColourPrimaries, 9)
	elemU(&colour, mkv.IDColourRange, 1)

	var video bytes.Buffer
	elemU(&video, mkv.IDPixelWidth, 320)
	elemU(&video, mkv.IDPixelHeight, 240)
	elemMaster(&video, mkv.IDColour, colour.Bytes())

	var trackEntry bytes.Buffer
	elemU(&trackEntry, mkv.IDTrackNumber, 1)
	elemU(&trackEntry, mkv.IDTrackUID, 1)
	elemU(&trackEntry, mkv.IDTrackType, mkv.TrackTypeVideo)
	elemS(&trackEntry, mkv.IDCodecID, "V_MPEG4/ISO/AVC")
	elemMaster(&trackEntry, mkv.IDVideo, video.Bytes())

	var tracks bytes.Buffer
	elemMaster(&tracks, mkv.IDTrackEntry, trackEntry.Bytes())

	var info bytes.Buffer
	elemU(&info, mkv.IDTimecodeScale, 1_000_000)

	// Attachments > AttachedFile
	var attachedFile bytes.Buffer
	elemS(&attachedFile, mkv.IDFileName, "font.ttf")
	elemS(&attachedFile, mkv.IDFileMimeType, "application/x-truetype-font")
	elemB(&attachedFile, mkv.IDFileData, []byte{1, 2, 3, 4})
	elemU(&attachedFile, mkv.IDFileUID, 42)
	var attachments bytes.Buffer
	elemMaster(&attachments, mkv.IDAttachedFile, attachedFile.Bytes())

	// Tags > Tag > {Targets, SimpleTag}
	var simpleTag bytes.Buffer
	elemS(&simpleTag, mkv.IDTagName, "TITLE")
	elemS(&simpleTag, mkv.IDTagString, "Hello")
	var targets bytes.Buffer
	elemS(&targets, mkv.IDTargetType, "MOVIE")
	elemU(&targets, mkv.IDTagTrackUID, 1)
	var tag bytes.Buffer
	elemMaster(&tag, mkv.IDTargets, targets.Bytes())
	elemMaster(&tag, mkv.IDSimpleTag, simpleTag.Bytes())
	var tags bytes.Buffer
	elemMaster(&tags, mkv.IDTag, tag.Bytes())

	var seg bytes.Buffer
	elemMaster(&seg, mkv.IDInfo, info.Bytes())
	elemMaster(&seg, mkv.IDTracks, tracks.Bytes())
	elemMaster(&seg, mkv.IDAttachments, attachments.Bytes())
	elemMaster(&seg, mkv.IDTags, tags.Bytes())

	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	elemMaster(&full, mkv.IDSegment, seg.Bytes())

	c, _, err := ReadStream(context.Background(), &readerOnly{r: bytes.NewReader(full.Bytes())})
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}

	// Colour
	if len(c.Tracks) != 1 {
		t.Fatalf("got %d tracks, want 1", len(c.Tracks))
	}
	tr := c.Tracks[0]
	if tr.ColorSpace == nil || *tr.ColorSpace != 9 ||
		tr.ColorTransfer == nil || *tr.ColorTransfer != 16 ||
		tr.ColorPrimaries == nil || *tr.ColorPrimaries != 9 ||
		tr.ColorRange == nil || *tr.ColorRange != 1 {
		t.Errorf("colour not parsed: space=%v transfer=%v prim=%v range=%v",
			tr.ColorSpace, tr.ColorTransfer, tr.ColorPrimaries, tr.ColorRange)
	}

	// Attachment
	if len(c.Attachments) != 1 {
		t.Fatalf("got %d attachments, want 1", len(c.Attachments))
	}
	a := c.Attachments[0]
	if a.Name != "font.ttf" || a.MIMEType != "application/x-truetype-font" || len(a.Data) != 4 {
		t.Errorf("attachment wrong: %+v", a)
	}

	// Tag
	if len(c.Tags) != 1 || len(c.Tags[0].SimpleTags) != 1 {
		t.Fatalf("tags = %+v", c.Tags)
	}
	st := c.Tags[0].SimpleTags[0]
	if st.Name != "TITLE" || st.Value != "Hello" {
		t.Errorf("simple tag = %+v", st)
	}
	if c.Tags[0].TargetType != "MOVIE" || c.Tags[0].TargetID != 1 {
		t.Errorf("tag targets = type %q id %d", c.Tags[0].TargetType, c.Tags[0].TargetID)
	}
}
