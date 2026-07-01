package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

var jpegStub = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}

// buildMKVWithCover writes a 1-track MKV carrying image attachments.
func buildMKVWithCover(t *testing.T, atts []mkv.Attachment) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info:        mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"},
		Attachments: atts,
	}
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
		Width: u32p(320), Height: u32p(240)}}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, 100); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteClusterWithCues(0, 1_000_000, []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x00, 0x00, 0x00, 0x01, 0x65}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// An MKV cover-art attachment survives the round trip: to-mp4 carries it as
// the iTunes covr atom, OpenMeta/from-mp4 bring it back as an attachment.
func TestCoverArtRoundTrip(t *testing.T) {
	src := buildMKVWithCover(t, []mkv.Attachment{
		{ID: 7, Name: "cover.jpg", MIMEType: "image/jpeg", Data: jpegStub},
	})
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), src, out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("covr")) {
		t.Fatal("MP4 has no covr atom")
	}

	c, _, err := OpenMeta(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Attachments) != 1 {
		t.Fatalf("attachments = %+v, want the cover back", c.Attachments)
	}
	att := c.Attachments[0]
	if att.MIMEType != "image/jpeg" || att.Name != "cover.jpg" || !bytes.Equal(att.Data, jpegStub) {
		t.Errorf("cover = %+v, want image/jpeg cover.jpg with identical bytes", att)
	}
}

// pickCoverArt prefers a "cover.*"-named image over other image attachments and
// ignores non-image ones (fonts).
func TestPickCoverArt(t *testing.T) {
	font := mkv.Attachment{Name: "font.ttf", MIMEType: "font/ttf", Data: []byte("f")}
	other := mkv.Attachment{Name: "poster.png", MIMEType: "image/png", Data: []byte("p")}
	cover := mkv.Attachment{Name: "Cover.jpg", MIMEType: "image/jpeg", Data: []byte("c")}

	if got := pickCoverArt([]mkv.Attachment{font}); got != nil {
		t.Errorf("font-only: got %+v, want nil", got)
	}
	if got := pickCoverArt([]mkv.Attachment{font, other, cover}); got == nil || got.png || !bytes.Equal(got.data, []byte("c")) {
		t.Errorf("cover.* must win over other images, got %+v", got)
	}
	if got := pickCoverArt([]mkv.Attachment{font, other}); got == nil || !got.png {
		t.Errorf("fallback to the first image attachment failed, got %+v", got)
	}
}
