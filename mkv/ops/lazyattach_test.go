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

// describedFixture is a two-track file whose one attachment carries a description, written through EditMetadata.
func describedFixture(t *testing.T, dir, name, desc string) string {
	t.Helper()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	src := buildMinimalMKV(t, dir, name, tracks, append(testBlocks(1), testBlocks(2)...), 4000)
	out := filepath.Join(dir, "described-"+name)
	mustNil(t, EditMetadata(context.Background(), src, out, func(c *mkv.Container) {
		c.Attachments = []mkv.Attachment{{ID: 1, Name: "a.ttf", MIMEType: "font/ttf", Description: desc, Data: []byte("font-bytes")}}
	}))
	return out
}

// attachmentDescription is the description of the file's one attachment named a.ttf.
func attachmentDescription(t *testing.T, path string) string {
	t.Helper()
	c, err := reader.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range c.Attachments {
		if a.Name == "a.ttf" {
			return a.Description
		}
	}
	t.Fatalf("%s: no attachment a.ttf among %d", filepath.Base(path), len(c.Attachments))
	return ""
}

// TestAttachmentDescriptionSurvivesEveryRewrite pins that an attachment's FileDescription is read, and comes back
// from each op that rewrites the attachments: EditMetadata, EditInPlace, RemoveTrack, AddTrack, MergeSubtitle,
// MergeASS, Merge, Mux, Join, Split, AddAttachment and RemoveAttachment.
func TestAttachmentDescriptionSurvivesEveryRewrite(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	const desc = "the font the subtitles use"
	a, b := describedFixture(t, dir, "a.mkv", desc), describedFixture(t, dir, "b.mkv", desc)
	if got := attachmentDescription(t, a); got != desc {
		t.Fatalf("read back from the writer: %q, want %q", got, desc)
	}
	lazy, err := reader.OpenWithFS(ctx, a, mkv.FSFrom(nil), reader.WithoutAttachmentData())
	if err != nil {
		t.Fatal(err)
	}
	srt, ass := filepath.Join(dir, "sub.srt"), filepath.Join(dir, "sub.ass")
	mustNil(t, os.WriteFile(srt, []byte("1\n00:00:00,500 --> 00:00:01,500\nLine\n\n"), 0o644))
	mustNil(t, os.WriteFile(ass, []byte("[Script Info]\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:00.50,0:00:01.50,Default,,0,0,0,,Line\n"), 0o644))
	ops := map[string]func(dst string) error{
		"edit-metadata": func(dst string) error {
			return EditMetadata(ctx, a, dst, func(c *mkv.Container) { c.Info.Title = "edited" })
		},
		"edit-in-place": func(dst string) error {
			src, err := os.ReadFile(a)
			if err != nil {
				return err
			}
			if err := os.WriteFile(dst, src, 0o644); err != nil {
				return err
			}
			return EditInPlace(ctx, dst, func(c *mkv.Container) { c.Info.Title = "edited in place" })
		},
		"remove-track": func(dst string) error { return RemoveTrack(ctx, a, dst, []uint64{2}) },
		"add-track": func(dst string) error {
			return AddTrack(ctx, a, dst, mkv.TrackInput{SourcePath: b, TrackID: 2, Language: "fre"})
		},
		"merge-subtitle": func(dst string) error { return MergeSubtitle(ctx, a, srt, dst, "eng", "Sub") },
		"merge-ass":      func(dst string) error { return MergeASS(ctx, a, ass, dst, "eng", "Sub") },
		"merge": func(dst string) error {
			return Merge(ctx, mkv.MergeOptions{OutputPath: dst, Inputs: []mkv.MergeInput{{SourcePath: a, TrackIDs: []uint64{1, 2}}}})
		},
		"mux": func(dst string) error {
			return Mux(ctx, mkv.MuxOptions{OutputPath: dst, Tracks: []mkv.TrackInput{{SourcePath: a, TrackID: 1}}, Attachments: lazy.Attachments})
		},
		"join": func(dst string) error { return Join(ctx, []string{a, b}, dst) },
		"split": func(dst string) error {
			parts, err := Split(ctx, mkv.SplitOptions{SourcePath: a, OutputDir: filepath.Dir(dst), Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 2000}}})
			if err != nil {
				return err
			}
			return os.Rename(parts[0], dst)
		},
		"add-attachment": func(dst string) error {
			return AddAttachment(ctx, a, dst, mkv.Attachment{Name: "b.bin", MIMEType: "application/octet-stream", Data: []byte("more")})
		},
		"remove-attachment": func(dst string) error {
			two := filepath.Join(filepath.Dir(dst), "two.mkv")
			if err := AddAttachment(ctx, a, two, mkv.Attachment{Name: "b.bin", MIMEType: "application/octet-stream", Data: []byte("more")}); err != nil {
				return err
			}
			return RemoveAttachment(ctx, two, dst, "b.bin")
		},
	}
	for name, run := range ops {
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), name+".mkv")
			if err := run(dst); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got := attachmentDescription(t, dst); got != desc {
				t.Errorf("%s: description %q, want %q", name, got, desc)
			}
		})
	}
}

// TestAttachmentDescriptionEditedThroughTheAPIIsWritten pins that a description changed on the Attachment is what the
// rewrite writes, through EditMetadata and EditInPlace alike, and that one cleared is left out.
func TestAttachmentDescriptionEditedThroughTheAPIIsWritten(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	const old, edited = "the font the subtitles use", "the font the titles use"
	a := describedFixture(t, dir, "a.mkv", old)
	edit := func(desc string) func(*mkv.Container) {
		return func(c *mkv.Container) { c.Attachments[0].Description = desc }
	}
	t.Run("edit-metadata", func(t *testing.T) {
		dst := filepath.Join(dir, "edited.mkv")
		mustNil(t, EditMetadata(ctx, a, dst, edit(edited)))
		if got := attachmentDescription(t, dst); got != edited {
			t.Errorf("description %q, want %q", got, edited)
		}
	})
	t.Run("edit-in-place", func(t *testing.T) {
		dst := filepath.Join(dir, "inplace.mkv")
		src, err := os.ReadFile(a)
		mustNil(t, err)
		mustNil(t, os.WriteFile(dst, src, 0o644))
		mustNil(t, EditInPlace(ctx, dst, edit(edited)))
		if got := attachmentDescription(t, dst); got != edited {
			t.Errorf("description %q, want %q", got, edited)
		}
	})
	t.Run("cleared-is-left-out", func(t *testing.T) {
		dst := filepath.Join(dir, "cleared.mkv")
		mustNil(t, EditMetadata(ctx, a, dst, edit("")))
		if got := attachmentDescription(t, dst); got != "" {
			t.Errorf("description %q, want none", got)
		}
		raw, err := os.ReadFile(dst)
		mustNil(t, err)
		if bytes.Contains(raw, []byte(old)) {
			t.Error("the cleared description is still in the file")
		}
	})
}
