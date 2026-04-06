package matroska

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveTrack(t *testing.T) {
	c := requireFixture(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "removed.mkv")

	assertNoErr(t, RemoveTrack(context.Background(), fixturePath, out, []uint64{2}))

	got, err := Open(context.Background(), out)
	assertNoErr(t, err)
	assertEqual(t, len(got.Tracks), len(c.Tracks)-1, "tracks count")
	assertEqual(t, got.Tracks[0].Type, VideoTrack, "remaining track type")

	counts := countBlocks(t, out, got.Info.TimecodeScale)
	if counts[1] == 0 {
		t.Error("no video blocks")
	}
	if counts[2] != 0 {
		t.Error("audio blocks should be gone")
	}
}

func TestRemoveTrackAllFails(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "empty.mkv")

	err := RemoveTrack(context.Background(), fixturePath, out, []uint64{1, 2})
	if err == nil {
		t.Fatal("expected error when removing all tracks")
	}
}

func TestAddTrack(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "added.mkv")

	assertNoErr(t, AddTrack(context.Background(), fixturePath, out, TrackInput{
		SourcePath: fixturePath,
		TrackID:    2,
		Language:   "fre",
		Name:       "French Copy",
		IsDefault:  false,
	}))

	got, err := Open(context.Background(), out)
	assertNoErr(t, err)
	assertEqual(t, len(got.Tracks), 3, "tracks count")
	assertEqual(t, got.Tracks[2].Language, "fre", "added track lang")
	assertEqual(t, got.Tracks[2].Name, "French Copy", "added track name")

	counts := countBlocks(t, out, got.Info.TimecodeScale)
	t.Logf("blocks per track: %v", counts)
	if counts[3] == 0 {
		t.Error("no blocks for added track")
	}
}

func TestEditMetadata(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "edited.mkv")

	assertNoErr(t, EditMetadata(context.Background(), fixturePath, out, func(c *Container) {
		c.Info.Title = "New Title"
	}))

	got, err := Open(context.Background(), out)
	assertNoErr(t, err)
	assertEqual(t, got.Info.Title, "New Title", "title")
	assertEqual(t, len(got.Tracks), 2, "tracks preserved")

	counts := countBlocks(t, out, got.Info.TimecodeScale)
	if counts[1] == 0 || counts[2] == 0 {
		t.Errorf("blocks missing: %v", counts)
	}
}

func TestExtractAttachment(t *testing.T) {
	// Build an MKV with an attachment
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "with_att.mkv")

	src, err := os.Create(srcPath)
	assertNoErr(t, err)
	data := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A}
	c := &Container{
		Info:        SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"},
		Attachments: []Attachment{{ID: 42, Name: "cover.png", MIMEType: "image/png", Data: data}},
	}
	assertNoErr(t, Write(src, c))
	src.Close()

	outFile := filepath.Join(dir, "extracted.png")
	assertNoErr(t, ExtractAttachment(context.Background(), srcPath, 42, outFile))

	got, err := os.ReadFile(outFile)
	assertNoErr(t, err)
	assertDeepEqual(t, got, data, "extracted data")
}

func TestExtractAttachmentNotFound(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	err := ExtractAttachment(context.Background(), fixturePath, 999, filepath.Join(dir, "nope"))
	if err == nil {
		t.Fatal("expected error for missing attachment")
	}
}
