package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

func trackEntryEl(num uint64) []byte {
	var te bytes.Buffer
	te.Write(uintEl(mkv.IDTrackNumber, num))
	te.Write(uintEl(mkv.IDTrackType, mkv.TrackTypeVideo))
	ebml.WriteElementHeader(&te, mkv.IDCodecID, 5)
	ebml.WriteString(&te, "V_VP9")
	return wrapEl(mkv.IDTrackEntry, te.Bytes())
}

func segmentOf(body []byte) []byte {
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(len(body)))
	buf.Write(body)
	return buf.Bytes()
}

// TestTailInfoAndTracksDoNotAddToTheHead: the spec allows one Info and one Tracks and the first wins, yet the tail
// a SeekHead-less file is read back from was decoded into the same container: a second Tracks there added its
// tracks to the head's, and a second Info reset the timestamp scale. Read now keeps the head's, as ReadMeta does.
func TestTailInfoAndTracksDoNotAddToTheHead(t *testing.T) {
	var seg bytes.Buffer
	seg.Write(wrapEl(mkv.IDInfo, uintElN(mkv.IDTimecodeScale, 500000, 4)))
	seg.Write(wrapEl(mkv.IDTracks, trackEntryEl(1)))
	seg.Write(realCluster())
	seg.Write(wrapEl(mkv.IDInfo, uintElN(mkv.IDTimecodeScale, 1000000, 4)))
	seg.Write(wrapEl(mkv.IDTracks, trackEntryEl(2)))
	seg.Write(wrapEl(mkv.IDCues, wrapEl(mkv.IDCuePoint, append(uintEl(mkv.IDCueTime, 0),
		wrapEl(mkv.IDCueTrackPositions, append(uintEl(mkv.IDCueTrack, 1), uintEl(mkv.IDCueClusterPos, 1)...))...))))
	data := segmentOf(seg.Bytes())
	c, err := Read(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tracks) != 1 || c.Info.TimecodeScale != 500000 {
		t.Errorf("Read: %d tracks, timestamp scale %d; want the head's 1 track and 500000", len(c.Tracks), c.Info.TimecodeScale)
	}
}

// TestUnknownSizeTracksBeforeTheRealOne: a Tracks of unknown size holds nothing decoded; ReadStream marked Tracks as
// read on it and skipped the real one after it as a duplicate, where Read reads it.
func TestUnknownSizeTracksBeforeTheRealOne(t *testing.T) {
	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDTracks, -1)
	seg.Write(wrapEl(mkv.IDTracks, trackEntryEl(1)))
	seg.Write(realCluster())
	data := segmentOf(seg.Bytes())
	cs, err := Read(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	cst, _, err := ReadStream(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Tracks) != 1 || len(cst.Tracks) != 1 {
		t.Errorf("Read %d tracks, ReadStream %d; want 1 and 1", len(cs.Tracks), len(cst.Tracks))
	}
}

// TestElementAcrossTheSegmentEnd: a head element that starts inside the Segment and runs past its declared end is
// read by Read whole; ReadStream stopped there and lost it.
func TestElementAcrossTheSegmentEnd(t *testing.T) {
	var seg bytes.Buffer
	seg.Write(wrapEl(mkv.IDInfo, uintElN(mkv.IDTimecodeScale, 1000000, 4)))
	seg.Write(wrapEl(mkv.IDTracks, trackEntryEl(1)))
	tags := wrapEl(mkv.IDTags, wrapEl(mkv.IDTag, wrapEl(mkv.IDSimpleTag, mkvTagName("TITLE"))))
	declared := seg.Len() + 4 // the Segment ends four bytes into the Tags
	seg.Write(tags)
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(declared))
	buf.Write(seg.Bytes())
	cs, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	cst, _, err := ReadStream(context.Background(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Tags) != len(cst.Tags) {
		t.Errorf("Read %d tags, ReadStream %d", len(cs.Tags), len(cst.Tags))
	}
}
