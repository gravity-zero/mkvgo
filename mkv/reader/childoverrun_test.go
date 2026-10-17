package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestChildPastItsParent: a child declaring more bytes than its parent holds does not swallow what follows the
// parent: the Tags after a Tracks element whose last child overruns it are read, by Read and ReadStream alike.
func TestChildPastItsParent(t *testing.T) {
	var te bytes.Buffer
	te.Write(uintEl(mkv.IDTrackNumber, 1))
	te.Write(uintEl(mkv.IDTrackType, mkv.TrackTypeVideo))
	ebml.WriteElementHeader(&te, mkv.IDCodecID, 5)
	ebml.WriteString(&te, "V_VP9")
	var tracksBody bytes.Buffer
	tracksBody.Write(wrapEl(mkv.IDTrackEntry, te.Bytes()))
	ebml.WriteElementHeader(&tracksBody, mkv.IDVoid, 200) // declares 200 bytes, holds 2 inside Tracks
	tracksBody.Write([]byte{0, 0})
	tags := wrapEl(mkv.IDTags, wrapEl(mkv.IDTag, wrapEl(mkv.IDSimpleTag, mkvTagName("TITLE"))))

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)
	seg.Write(wrapEl(mkv.IDTracks, tracksBody.Bytes()))
	seg.Write(tags)
	seg.Write(realCluster())
	seg.Write(make([]byte, 256)) // room for the overrun, so neither reader meets the end of the file
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	cs, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	cst, _, err := ReadStream(context.Background(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*mkv.Container{"Read": cs, "ReadStream": cst} {
		if len(c.Tracks) != 1 || len(c.Tags) != 1 {
			t.Errorf("%s: %d tracks, %d tags; want 1 and 1", name, len(c.Tracks), len(c.Tags))
		}
	}
}
