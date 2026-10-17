package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

func uintElN(id uint32, v uint64, n int) []byte {
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, id, int64(n))
	ebml.WriteUint(&b, v, n)
	return b.Bytes()
}

// TestOutOfRangeValuesAreRejected: a value past its field's type is left unknown, not truncated into a plausible
// one (300 channels read as 44), through Read and ReadStream alike.
func TestOutOfRangeValuesAreRejected(t *testing.T) {
	entry := func(num uint64, typ uint64, codec string, body []byte) []byte {
		var te bytes.Buffer
		te.Write(uintEl(mkv.IDTrackNumber, num))
		te.Write(uintEl(mkv.IDTrackType, typ))
		ebml.WriteElementHeader(&te, mkv.IDCodecID, int64(len(codec)))
		ebml.WriteString(&te, codec)
		te.Write(body)
		return wrapEl(mkv.IDTrackEntry, te.Bytes())
	}
	colour := wrapEl(mkv.IDColour, append(uintElN(mkv.IDColourMatrix, 70000, 4), uintElN(mkv.IDColourBitsPerChannel, 1<<20, 4)...))
	video := wrapEl(mkv.IDVideo, append(append(uintElN(mkv.IDPixelWidth, 1<<33, 8), uintElN(mkv.IDPixelHeight, 720, 2)...), colour...))
	audio := wrapEl(mkv.IDAudio, append(uintElN(mkv.IDChannels, 300, 2), uintElN(mkv.IDBitDepth, 512, 2)...))
	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)
	seg.Write(wrapEl(mkv.IDTracks, append(entry(1, mkv.TrackTypeVideo, "V_VP9", video), entry(2, mkv.TrackTypeAudio, "A_AAC", audio)...)))
	seg.Write(realCluster())
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
		if len(c.Tracks) != 2 {
			t.Fatalf("%s: %d tracks", name, len(c.Tracks))
		}
		v, a := c.Tracks[0], c.Tracks[1]
		if v.Width != nil || v.Height == nil || *v.Height != 720 || v.ColorSpace != nil || v.VideoBitDepth != nil {
			t.Errorf("%s: width %v height %v matrix %v depth %v; want nil, 720, nil, nil", name, v.Width, v.Height, v.ColorSpace, v.VideoBitDepth)
		}
		if a.Channels != nil || a.BitDepth != nil {
			t.Errorf("%s: channels %v bit depth %v; want both left unknown", name, a.Channels, a.BitDepth)
		}
	}
}
