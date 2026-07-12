package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestStreamSeekableParity verifies the streaming parser (ReadStream) captures
// the same track ContentEncodings (HeaderStripping) and nested SimpleTags as the
// seekable parser (Read) - the drift the audit flagged.
func TestStreamSeekableParity(t *testing.T) {
	headerStrip := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	// Tracks > TrackEntry > ContentEncodings > ContentEncoding > ContentCompression > ContentCompSettings
	comp := wrapEl(mkv.IDContentCompSettings, headerStrip)
	enc := wrapEl(mkv.IDContentCompression, comp)
	enc = wrapEl(mkv.IDContentEncoding, enc)
	ce := wrapEl(mkv.IDContentEncodings, enc)

	var te bytes.Buffer
	ebml.WriteElementHeader(&te, mkv.IDTrackNumber, 1)
	ebml.WriteUint(&te, 1, 1)
	ebml.WriteElementHeader(&te, mkv.IDTrackType, 1)
	ebml.WriteUint(&te, mkv.TrackTypeVideo, 1)
	ebml.WriteElementHeader(&te, mkv.IDCodecID, 5)
	ebml.WriteString(&te, "V_VP9")
	te.Write(ce)
	tracks := wrapEl(mkv.IDTracks, wrapEl(mkv.IDTrackEntry, te.Bytes()))

	// Tags > Tag > SimpleTag{ TagName, TagBinary, SimpleTag{ TagName } }
	innerST := wrapEl(mkv.IDSimpleTag, mkvTagName("SUB"))
	var st bytes.Buffer
	st.Write(mkvTagName("OUTER"))
	ebml.WriteElementHeader(&st, mkv.IDTagBinary, 2)
	ebml.WriteBytes(&st, []byte{0x01, 0x02})
	st.Write(innerST)
	tags := wrapEl(mkv.IDTags, wrapEl(mkv.IDTag, wrapEl(mkv.IDSimpleTag, st.Bytes())))

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)
	seg.Write(tracks)
	seg.Write(tags)
	seg.Write(realCluster()) // so ReadStream reaches a cluster and returns

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	data := buf.Bytes()

	cs, err := Read(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	cst, _, err := ReadStream(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}

	// HeaderStripping parity (and non-empty in the seekable baseline)
	if len(cs.Tracks) != 1 || !bytes.Equal(cs.Tracks[0].HeaderStripping, headerStrip) {
		t.Fatalf("seekable HeaderStripping = %v, want %v", trackStrip(cs), headerStrip)
	}
	if !bytes.Equal(trackStrip(cst), trackStrip(cs)) {
		t.Errorf("streaming HeaderStripping = %v, seekable = %v (parity broken)", trackStrip(cst), trackStrip(cs))
	}

	// Nested SimpleTag parity
	if nSub(cs) != 1 {
		t.Fatalf("seekable nested subtags = %d, want 1", nSub(cs))
	}
	if nSub(cst) != nSub(cs) {
		t.Errorf("streaming nested subtags = %d, seekable = %d (parity broken)", nSub(cst), nSub(cs))
	}
}

func wrapEl(id uint32, body []byte) []byte {
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, id, int64(len(body)))
	b.Write(body)
	return b.Bytes()
}

func mkvTagName(s string) []byte {
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, mkv.IDTagName, int64(len(s)))
	ebml.WriteString(&b, s)
	return b.Bytes()
}

func trackStrip(c *mkv.Container) []byte {
	if len(c.Tracks) == 0 {
		return nil
	}
	return c.Tracks[0].HeaderStripping
}

func nSub(c *mkv.Container) int {
	if len(c.Tags) == 0 || len(c.Tags[0].SimpleTags) == 0 {
		return -1
	}
	return len(c.Tags[0].SimpleTags[0].SubTags)
}

// TestStreamSeekableParitySubChapters verifies the streaming parser captures
// nested ChapterAtoms (SubChapters) identically to the seekable parser.
func TestStreamSeekableParitySubChapters(t *testing.T) {
	chapUID := func(id byte) []byte {
		var b bytes.Buffer
		ebml.WriteElementHeader(&b, mkv.IDChapterUID, 1)
		ebml.WriteUint(&b, uint64(id), 1)
		return b.Bytes()
	}
	// ChapterAtom{ UID, ChapterAtom{ UID } }
	var outer bytes.Buffer
	outer.Write(chapUID(1))
	outer.Write(wrapEl(mkv.IDChapterAtom, chapUID(2)))
	chapters := wrapEl(mkv.IDChapters, wrapEl(mkv.IDEditionEntry, wrapEl(mkv.IDChapterAtom, outer.Bytes())))

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)
	seg.Write(chapters)
	seg.Write(realCluster())
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	data := buf.Bytes()

	cs, err := Read(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	cst, _, err := ReadStream(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	nSub := func(c *mkv.Container) int {
		if len(c.Chapters) == 0 {
			return -1
		}
		return len(c.Chapters[0].SubChapters)
	}
	if nSub(cs) != 1 {
		t.Fatalf("seekable sub-chapters = %d, want 1", nSub(cs))
	}
	if nSub(cst) != nSub(cs) {
		t.Errorf("streaming sub-chapters = %d, seekable = %d (parity broken)", nSub(cst), nSub(cs))
	}
}
