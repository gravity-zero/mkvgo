package ops

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

// TestOpsRefuseAnOutputThatIsAnInput pins that every Matroska writer refuses to write over a file it reads, as the
// MP4 engine does: EditMetadata(p, p) truncated the film, failed reading it back, then removed it.
func TestOpsRefuseAnOutputThatIsAnInput(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "film.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}, []mkv.Track{videoTrack(1), audioTrack(2)}, 2000))
	for tc := int64(0); tc < 2000; tc += 500 {
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{
			{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: []byte("v")},
			{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")},
		}))
	}
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())
	srt := filepath.Join(dir, "subs.srt")
	mustNil(t, os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,000\nhi\n"), 0o644))
	original, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.mkv")
	mustNil(t, os.WriteFile(other, original, 0o644))
	ctx := context.Background()
	ops := map[string]func() error{
		"EditMetadata": func() error { return EditMetadata(ctx, src, src, func(c *mkv.Container) { c.Info.Title = "x" }) },
		"RemoveTrack":  func() error { return RemoveTrack(ctx, src, src, []uint64{2}) },
		"AddTrack":     func() error { return AddTrack(ctx, src, src, mkv.TrackInput{SourcePath: src, TrackID: 2}) },
		"Join":         func() error { return Join(ctx, []string{src, src}, src) },
		"Mux": func() error {
			return Mux(ctx, mkv.MuxOptions{OutputPath: src, Tracks: []mkv.TrackInput{{SourcePath: src, TrackID: 1}}})
		},
		"MergeSubtitle":            func() error { return MergeSubtitle(ctx, src, srt, src, "eng", "") },
		"RemuxToWebM":              func() error { return RemuxToWebM(ctx, src, src) },
		"Reindex":                  func() error { return Reindex(ctx, src, src) },
		"Salvage":                  func() error { _, err := Salvage(ctx, src, src); return err },
		"AddTrack from the output": func() error { return AddTrack(ctx, other, src, mkv.TrackInput{SourcePath: src, TrackID: 2}) },
		"ExtractAttachment":        func() error { return ExtractAttachment(ctx, src, 1, src) },
		"Merge, attachments only": func() error {
			return Merge(ctx, mkv.MergeOptions{OutputPath: src, Inputs: []mkv.MergeInput{{SourcePath: src, TrackIDs: []uint64{99}}, {SourcePath: other}}})
		},
		"MergeWithSubtitles, extras": func() error {
			return MergeWithSubtitles(ctx, src, srt, src, "eng", "", []mkv.MergeInput{{SourcePath: other}})
		},
	}
	for name, op := range ops {
		if err := op(); err == nil {
			t.Errorf("%s with its source as output succeeded", name)
		}
		if got, err := os.ReadFile(src); err != nil || !bytes.Equal(got, original) {
			t.Fatalf("%s changed or removed its source (%v)", name, err)
		}
	}
}

// TestMuxRefusesAnOutputItsAttachmentsAreReadFrom pins that the files lazy attachments are copied from count as
// inputs: muxing another file's tracks with a.mkv's attachments into a.mkv truncated it, then removed it.
func TestMuxRefusesAnOutputItsAttachmentsAreReadFrom(t *testing.T) {
	dir := t.TempDir()
	write := func(path string) {
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"},
			Attachments: []mkv.Attachment{{ID: 1, Name: "font.ttf", MIMEType: "font/ttf", Data: bytes.Repeat([]byte{7}, 4096)}}}
		mw := writer.NewMKVWriter(f)
		mustNil(t, mw.WriteStart())
		mustNil(t, mw.WriteMetadata(c, []mkv.Track{videoTrack(1)}, 1000))
		mustNil(t, mw.WriteClusterWithCues(0, 1_000_000, []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")}}))
		mustNil(t, mw.Finalize())
		mustNil(t, f.Close())
	}
	a, b := filepath.Join(dir, "a.mkv"), filepath.Join(dir, "b.mkv")
	write(a)
	write(b)
	ctx := context.Background()
	src, err := reader.OpenWithFS(ctx, a, nil, reader.WithoutAttachmentData())
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(a)
	err = Mux(ctx, mkv.MuxOptions{OutputPath: a, Tracks: []mkv.TrackInput{{SourcePath: b, TrackID: 1}}, Attachments: src.Attachments})
	if err == nil {
		t.Error("a mux writing over the file its attachments are read from succeeded")
	}
	if after, err := os.ReadFile(a); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the attachments' file was changed or removed (%v)", err)
	}
	err = EditMetadata(ctx, b, a, func(c *mkv.Container) { c.Attachments = append(c.Attachments, src.Attachments...) })
	if err == nil {
		t.Error("an edit writing over the file an added attachment is read from succeeded")
	}
	if after, err := os.ReadFile(a); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the added attachment's file was changed or removed (%v)", err)
	}
}

// TestMergeWithSubtitlesKeepsAFileNamedLikeItsTemp pins that the temporary merge next to the output never takes a
// file that exists: a base named out.mkv.tmp was removed by the temporary file's cleanup.
func TestMergeWithSubtitlesKeepsAFileNamedLikeItsTemp(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "out.mkv.tmp")
	f, err := os.Create(base)
	if err != nil {
		t.Fatal(err)
	}
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}, []mkv.Track{videoTrack(1)}, 1000))
	mustNil(t, mw.WriteClusterWithCues(0, 1_000_000, []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")}}))
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())
	other := filepath.Join(dir, "other.mkv")
	data, _ := os.ReadFile(base)
	mustNil(t, os.WriteFile(other, data, 0o644))
	srt := filepath.Join(dir, "s.srt")
	mustNil(t, os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,000\nhi\n"), 0o644))
	err = MergeWithSubtitles(context.Background(), base, srt, filepath.Join(dir, "out.mkv"), "eng", "", []mkv.MergeInput{{SourcePath: other}})
	if err == nil {
		t.Error("a merge whose temporary file is its base succeeded")
	}
	if after, err := os.ReadFile(base); err != nil || !bytes.Equal(after, data) {
		t.Fatalf("the base named like the temporary file was changed or removed (%v)", err)
	}
}
