package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestResyncFindingNothingCountsTheJunk: a read that passed junk over and found nothing behind it reported no
// resynced byte, so the file looked sound to Validate and Diagnose and to a reader comparing notes with a stream.
func TestResyncFindingNothingCountsTheJunk(t *testing.T) {
	info := wrapEl(mkv.IDInfo, uintEl(mkv.IDTimecodeScale, 1000000))
	junk := append(wrapEl(mkv.IDTrackEntry, []byte{0x30}), 0x41, 0x30, 0x85, 1, 2, 3, 4, 5) // no segment-level element follows
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&b, mkv.IDSegment, int64(len(info)+len(junk)))
	b.Write(info)
	b.Write(junk)
	want := int64(len(junk))
	c, err := Read(context.Background(), bytes.NewReader(b.Bytes()), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if c.ResyncedBytes != want {
		t.Errorf("Read: ResyncedBytes %d, want the %d junk bytes it passed over", c.ResyncedBytes, want)
	}
	m, err := ReadMeta(context.Background(), bytes.NewReader(b.Bytes()), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if m.ResyncedBytes != want {
		t.Errorf("ReadMeta: ResyncedBytes %d, want %d", m.ResyncedBytes, want)
	}
}
