package reader

import (
	"bytes"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// TestRewriteWritesOnlyWhatTheContainerStated pins that colour and display dimensions the reader took from the
// codec's SPS, where the container stated none, are not written into a rewritten container, while a value an edit
// sets is.
func TestRewriteWritesOnlyWhatTheContainerStated(t *testing.T) {
	w, h := uint32(1280), uint32(720)
	for name, avcC := range map[string][]byte{"colour": buildHighSPSAvcC(1, 1, 1), "aspect": buildSPSAspectAvcC(255, 4, 3)} {
		tr := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcC, Width: &w, Height: &h}
		var src bytes.Buffer
		if err := writer.WriteTracks(&src, []mkv.Track{tr}); err != nil {
			t.Fatal(err)
		}
		read := parseTracksOf(t, src.Bytes())
		if read.ColorPrimaries == nil && read.DisplayWidth == nil {
			t.Fatalf("%s: the reader took nothing from the SPS", name)
		}
		var out bytes.Buffer
		if err := writer.WriteTracks(&out, []mkv.Track{read}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out.Bytes(), src.Bytes()) {
			t.Errorf("%s: the rewrite added what the SPS says to the container (%d bytes, source %d)", name, out.Len(), src.Len())
		}
	}
	tr := parseTracksOf(t, func() []byte {
		var b bytes.Buffer
		_ = writer.WriteTracks(&b, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: buildHighSPSAvcC(1, 1, 1), Width: &w, Height: &h}})
		return b.Bytes()
	}())
	nine := uint16(9)
	tr.ColorPrimaries = &nine
	if got := parseTracksOf(t, func() []byte { var b bytes.Buffer; _ = writer.WriteTracks(&b, []mkv.Track{tr}); return b.Bytes() }()); got.ColorPrimaries == nil || *got.ColorPrimaries != 9 {
		t.Errorf("an edited primaries value was not written: %v", got.ColorPrimaries)
	}
}

// parseTracksOf reads the one track of a serialised Tracks element.
func parseTracksOf(t *testing.T, tracks []byte) mkv.Track {
	t.Helper()
	p := &parser{r: bytes.NewReader(tracks), segEnd: -1, metaBudget: 1 << 20}
	h, _, err := p.readHeader()
	if err != nil {
		t.Fatal(err)
	}
	var c mkv.Container
	if err := p.parseTracks(h.Size, &c); err != nil || len(c.Tracks) != 1 {
		t.Fatalf("parse: %v, %d tracks", err, len(c.Tracks))
	}
	return c.Tracks[0]
}
