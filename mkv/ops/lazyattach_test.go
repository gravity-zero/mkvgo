package ops

// lazyattach_test.go pins that an op which merely CARRIES a source's
// attachments never loads them: the source is opened with the payloads left on
// disk, and the writer copies them through from there - bytes unchanged. The
// one op that adds an attachment writes the new one from Data and streams the
// old ones alongside.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// lazyFixture is a two-track file carrying one 300 KB attachment.
func lazyFixture(t *testing.T, dir string) (path string, font []byte) {
	t.Helper()
	font = bytes.Repeat([]byte("font-bytes-"), 30_000)
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	src := filepath.Join(dir, "src.mkv")
	c := &mkv.Container{
		Info:        mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test"},
		Attachments: []mkv.Attachment{{ID: 1, Name: "a.ttf", MIMEType: "font/ttf", Data: font, Size: int64(len(font))}},
	}
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteMetadata(c, tracks, 4000); err != nil {
		t.Fatal(err)
	}
	var blocks []mkv.Block
	for i := int64(0); i < 4; i++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: i * 1000, Keyframe: true, Data: []byte{0xAA}},
			mkv.Block{TrackNumber: 2, Timecode: i * 1000, Keyframe: true, Data: []byte{0x01}})
	}
	pos := mw.RelPos()
	if err := writer.WriteCluster(f, 0, 1_000_000, blocks); err != nil {
		t.Fatal(err)
	}
	mw.Cues = []mkv.CuePoint{{TimeMs: 0, Track: 1, ClusterPos: pos}}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return src, font
}

func attachmentBytes(t *testing.T, path string) [][]byte {
	t.Helper()
	c, err := reader.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, a := range c.Attachments {
		out = append(out, a.Data)
	}
	return out
}

func TestAttachmentsAreCarriedWithoutBecomingResident(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src, font := lazyFixture(t, dir)

	// The source, opened the way the carrying ops open it, holds no payload.
	lazy, err := reader.OpenWithFS(ctx, src, mkv.FSFrom(nil), reader.WithoutAttachmentData())
	if err != nil {
		t.Fatal(err)
	}
	if len(lazy.Attachments) != 1 || lazy.Attachments[0].Data != nil || lazy.Attachments[0].DataPath == "" || lazy.Attachments[0].Size != int64(len(font)) {
		t.Fatalf("lazy open: %+v, want Data nil with DataPath/Size set", lazy.Attachments)
	}

	ops := []struct {
		name string
		run  func(dst string) error
	}{
		{"remove-track", func(dst string) error { return RemoveTrack(ctx, src, dst, []uint64{2}) }},
		{"edit-metadata", func(dst string) error {
			return EditMetadata(ctx, src, dst, func(c *mkv.Container) { c.Info.Title = "edited" })
		}},
		{"split", func(dst string) error {
			parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Dir(dst), Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 4000}}})
			if err != nil {
				return err
			}
			if len(parts) != 1 {
				t.Fatalf("split: %d parts", len(parts))
			}
			return os.Rename(parts[0], dst)
		}},
	}
	for _, op := range ops {
		dst := filepath.Join(dir, op.name+".mkv")
		if op.name == "split" {
			dst = filepath.Join(t.TempDir(), "split.mkv")
		}
		if err := op.run(dst); err != nil {
			t.Fatalf("%s: %v", op.name, err)
		}
		got := attachmentBytes(t, dst)
		if len(got) != 1 || !bytes.Equal(got[0], font) {
			t.Errorf("%s: attachment not carried byte for byte (%d attachment(s))", op.name, len(got))
		}
	}

	// A callback that needs the bytes asks for them: the inherited attachment
	// arrives without its Data, LoadAttachmentData fills it from disk.
	var seen []byte
	if err := EditMetadata(ctx, src, filepath.Join(dir, "loaded.mkv"), func(c *mkv.Container) {
		a := &c.Attachments[0]
		if a.Data != nil {
			t.Errorf("callback: Data already loaded (%d bytes) - the carrying open must leave it on disk", len(a.Data))
		}
		if err := LoadAttachmentData(ctx, a); err != nil {
			t.Errorf("LoadAttachmentData: %v", err)
		}
		seen = append([]byte(nil), a.Data...)
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seen, font) {
		t.Errorf("LoadAttachmentData: %d bytes, want the %d-byte font", len(seen), len(font))
	}

	// Adding one: the new payload from Data, the old one streamed from disk.
	dst := filepath.Join(dir, "added.mkv")
	extra := []byte("second-attachment")
	if err := AddAttachment(ctx, src, dst, mkv.Attachment{Name: "b.txt", MIMEType: "text/plain", Data: extra}); err != nil {
		t.Fatal(err)
	}
	got := attachmentBytes(t, dst)
	if len(got) != 2 || !bytes.Equal(got[0], font) || !bytes.Equal(got[1], extra) {
		t.Errorf("add-attachment: got %d attachment(s), want the carried font and the new one", len(got))
	}
}
