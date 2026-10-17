package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestStreamReadsPastAnUnknownSizeElement: an unknown-size SeekHead or element ahead of Tracks does not end the
// metadata scan: its children, no segment-level elements, are passed over and the Tracks behind them are read.
func TestStreamReadsPastAnUnknownSizeElement(t *testing.T) {
	var te bytes.Buffer
	te.Write(uintEl(mkv.IDTrackNumber, 1))
	te.Write(uintEl(mkv.IDTrackType, mkv.TrackTypeVideo))
	ebml.WriteElementHeader(&te, mkv.IDCodecID, 5)
	ebml.WriteString(&te, "V_VP9")
	tracks := wrapEl(mkv.IDTracks, wrapEl(mkv.IDTrackEntry, te.Bytes()))
	// 0x1B538667 is SignatureSlot, which the reader does not know.
	for name, id := range map[string]uint32{"SeekHead": mkv.IDSeekHead, "unknown element": 0x1B538667} {
		var seg bytes.Buffer
		ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)
		ebml.WriteElementID(&seg, id)
		seg.WriteByte(0xFF)          // unknown size
		seg.Write(uintEl(0x53AB, 1)) // a SeekID-like child
		seg.Write(tracks)
		seg.Write(realCluster())
		var buf bytes.Buffer
		writeEBMLHeader(&buf)
		writeSegmentStart(&buf, int64(seg.Len()))
		buf.Write(seg.Bytes())
		c, br, err := ReadStream(context.Background(), bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(c.Tracks) != 1 {
			t.Errorf("%s: %d tracks, want the one behind it", name, len(c.Tracks))
		}
		if b, err := br.Next(); err != nil || b.TrackNumber != 1 {
			t.Errorf("%s: first block %+v, %v", name, b, err)
		}
	}
}
