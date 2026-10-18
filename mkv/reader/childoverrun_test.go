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

// TestParentTooShortForItsLastChild: a parent declaring a byte less than its last child needs (a corrupted Info
// whose WritingApp runs into the Tracks) is the one whose size is wrong when the child's end, not the parent's,
// opens a segment-level element: the seekable reader reads the child and finds the Tracks right after it.
func TestParentTooShortForItsLastChild(t *testing.T) {
	var info bytes.Buffer
	info.Write(uintEl(mkv.IDTimecodeScale, 1000000))
	ebml.WriteElementHeader(&info, mkv.IDWritingApp, 6)
	ebml.WriteString(&info, "vpxenc")
	var te bytes.Buffer
	te.Write(uintEl(mkv.IDTrackNumber, 1))
	te.Write(uintEl(mkv.IDTrackType, mkv.TrackTypeVideo))
	ebml.WriteElementHeader(&te, mkv.IDCodecID, 5)
	ebml.WriteString(&te, "V_VP9")
	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, int64(info.Len()-1)) // a byte short of its WritingApp
	seg.Write(info.Bytes())
	seg.Write(wrapEl(mkv.IDTracks, wrapEl(mkv.IDTrackEntry, te.Bytes())))
	seg.Write(realCluster())
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	for name, read := range map[string]func() (*mkv.Container, error){
		"Read": func() (*mkv.Container, error) {
			return Read(context.Background(), bytes.NewReader(buf.Bytes()), "x.mkv")
		},
		"OpenMeta": func() (*mkv.Container, error) {
			return ReadMeta(context.Background(), bytes.NewReader(buf.Bytes()), "x.mkv")
		},
	} {
		c, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(c.Tracks) != 1 || c.Info.WritingApp != "vpxenc" {
			t.Errorf("%s: %d tracks, writing app %q; want the VP9 track and vpxenc", name, len(c.Tracks), c.Info.WritingApp)
		}
	}
}

// TestJunkAfterAShortParent: bytes left after a parent that declares too little, read as an unknown element of a
// large size, are junk to resync past, not a size to skip: the Tracks behind them are read by both seekable reads.
func TestJunkAfterAShortParent(t *testing.T) {
	var te bytes.Buffer
	te.Write(uintEl(mkv.IDTrackNumber, 1))
	te.Write(uintEl(mkv.IDTrackType, mkv.TrackTypeAudio))
	ebml.WriteElementHeader(&te, mkv.IDCodecID, 6)
	ebml.WriteString(&te, "A_OPUS")
	var seg bytes.Buffer
	seg.Write(wrapEl(mkv.IDInfo, uintEl(mkv.IDTimecodeScale, 1000000)))
	seg.Write([]byte{0x42, 0x99, 0x08, 0x00, 0x10, 0x00, 0x00}) // ID 0x4299, size 1 MiB: the rest of a longer Info
	seg.Write(wrapEl(mkv.IDTracks, wrapEl(mkv.IDTrackEntry, te.Bytes())))
	seg.Write(realCluster())
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	for name, read := range map[string]func() (*mkv.Container, error){
		"Read": func() (*mkv.Container, error) {
			return Read(context.Background(), bytes.NewReader(buf.Bytes()), "x.mkv")
		},
		"OpenMeta": func() (*mkv.Container, error) {
			return ReadMeta(context.Background(), bytes.NewReader(buf.Bytes()), "x.mkv")
		},
	} {
		c, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(c.Tracks) != 1 {
			t.Errorf("%s: %d tracks, want the Opus track behind the junk", name, len(c.Tracks))
		}
	}
}
