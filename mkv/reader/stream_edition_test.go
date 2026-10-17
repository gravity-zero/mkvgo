package reader

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

func uintEl(id uint32, v uint64) []byte {
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, id, 1)
	ebml.WriteUint(&b, v, 1)
	return b.Bytes()
}

// TestStreamEditionsKeepTheParserInStep: two editions carrying EditionUID and flags, then Tags and a Cluster, read
// the same through ReadStream as through Read, and the streaming BlockReader starts on the Cluster's block.
func TestStreamEditionsKeepTheParserInStep(t *testing.T) {
	edition := func(uid uint64, title string) []byte {
		var disp bytes.Buffer
		ebml.WriteElementHeader(&disp, mkv.IDChapString, int64(len(title)))
		ebml.WriteString(&disp, title)
		atom := append(append(uintEl(mkv.IDChapterUID, uid), uintEl(mkv.IDChapterTimeStart, 0)...), wrapEl(mkv.IDChapterDisplay, disp.Bytes())...)
		var ed bytes.Buffer
		ed.Write(uintEl(mkv.IDEditionUID, uid))
		ed.Write(uintEl(mkv.IDEditionFlagDefault, 1))
		ed.Write(uintEl(mkv.IDEditionFlagHidden, 0))
		ed.Write(uintEl(mkv.IDEditionFlagOrdered, 0))
		ed.Write(wrapEl(mkv.IDChapterAtom, atom))
		return wrapEl(mkv.IDEditionEntry, ed.Bytes())
	}
	chapters := wrapEl(mkv.IDChapters, append(edition(7, "one"), edition(9, "two")...))
	tags := wrapEl(mkv.IDTags, wrapEl(mkv.IDTag, wrapEl(mkv.IDSimpleTag, mkvTagName("TITLE"))))

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)
	seg.Write(chapters)
	seg.Write(tags)
	seg.Write(realCluster())
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	data := buf.Bytes()

	cs, err := Read(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	cst, br, err := ReadStream(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprintf("%+v %+v %+v", cst.Chapters, cst.OtherEditions, cst.Tags), fmt.Sprintf("%+v %+v %+v", cs.Chapters, cs.OtherEditions, cs.Tags); got != want {
		t.Errorf("ReadStream:\n%s\nRead:\n%s", got, want)
	}
	b, err := br.Next()
	if err != nil || b.TrackNumber != 1 || len(b.Data) != 1 || b.Data[0] != 0xAA {
		t.Errorf("first streamed block %+v, %v; want the Cluster's", b, err)
	}
}
