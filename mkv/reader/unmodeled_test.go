package reader

import (
	"bytes"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// TestRewriteKeepsTheTrackChildrenItDoesNotModel pins that a TrackEntry's, a Video's and an Audio's children the
// writer has no field for (FlagLacing, FlagOriginal, StereoMode 0, DisplayUnit, the Video FrameRate, Emphasis) come
// back from a read and a rewrite as they were.
func TestRewriteKeepsTheTrackChildrenItDoesNotModel(t *testing.T) {
	el := func(id uint32, body []byte) []byte {
		var b bytes.Buffer
		_, _ = ebml.WriteElementHeader(&b, id, int64(len(body)))
		b.Write(body)
		return b.Bytes()
	}
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	video := el(mkv.IDTrackEntry, cat(el(mkv.IDTrackNumber, []byte{1}), el(mkv.IDTrackType, []byte{1}), el(mkv.IDCodecID, []byte("V_VP9")),
		el(0x9C, []byte{0}), el(mkv.IDFlagOriginal, []byte{0}),
		el(mkv.IDVideo, cat(el(mkv.IDPixelWidth, []byte{64}), el(mkv.IDPixelHeight, []byte{64}), el(mkv.IDStereoMode, []byte{0}), el(0x54B2, []byte{3}), el(0x2383E3, []byte{0x41, 0xEF, 0xC2, 0x91})))))
	audio := el(mkv.IDTrackEntry, cat(el(mkv.IDTrackNumber, []byte{2}), el(mkv.IDTrackType, []byte{2}), el(mkv.IDCodecID, []byte("A_OPUS")),
		el(mkv.IDAudio, cat(el(mkv.IDChannels, []byte{2}), el(0x52F1, []byte{1})))))
	src := el(mkv.IDTracks, cat(video, audio))
	p := &parser{r: bytes.NewReader(src), segEnd: -1, metaBudget: 1 << 20}
	h, _, err := p.readHeader()
	if err != nil {
		t.Fatal(err)
	}
	var c mkv.Container
	if err := p.parseTracks(h.Size, &c); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writer.WriteTracks(&out, c.Tracks); err != nil {
		t.Fatal(err)
	}
	for _, child := range [][]byte{el(0x9C, []byte{0}), el(mkv.IDFlagOriginal, []byte{0}), el(mkv.IDStereoMode, []byte{0}), el(0x54B2, []byte{3}),
		el(0x2383E3, []byte{0x41, 0xEF, 0xC2, 0x91}), el(0x52F1, []byte{1})} {
		if !bytes.Contains(out.Bytes(), child) {
			t.Errorf("the rewrite lost % X", child)
		}
	}
}
