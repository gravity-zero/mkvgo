package matroska

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestFacadeAddAndRemoveAttachment proves both wrappers delegate: an
// attachment added to the fixture round-trips through Open, and removing it
// by name leaves the file without it.
func TestFacadeAddAndRemoveAttachment(t *testing.T) {
	requireFixture(t)
	ctx := context.Background()
	dir := t.TempDir()
	withAttachment := filepath.Join(dir, "with_attachment.mkv")

	att := Attachment{Name: "cover.png", MIMEType: "image/png", Data: []byte("fake-png-data")}
	assertNoErr(t, AddAttachment(ctx, fixturePath, withAttachment, att))

	c, err := Open(ctx, withAttachment)
	assertNoErr(t, err)
	if len(c.Attachments) != 1 || c.Attachments[0].Name != "cover.png" {
		t.Fatalf("Attachments = %+v, want one named cover.png", c.Attachments)
	}

	withoutAttachment := filepath.Join(dir, "without_attachment.mkv")
	assertNoErr(t, RemoveAttachment(ctx, withAttachment, withoutAttachment, "cover.png"))

	c2, err := Open(ctx, withoutAttachment)
	assertNoErr(t, err)
	if len(c2.Attachments) != 0 {
		t.Errorf("Attachments = %+v, want none after RemoveAttachment", c2.Attachments)
	}
}

// TestFacadeSetChapters proves SetChapters rewrites the destination with the
// given chapters.
func TestFacadeSetChapters(t *testing.T) {
	requireFixture(t)
	ctx := context.Background()
	dst := filepath.Join(t.TempDir(), "chaptered.mkv")

	chapters := []Chapter{
		{StartMs: 0, EndMs: 500, Title: "Intro"},
		{StartMs: 500, EndMs: 1021, Title: "Main"},
	}
	assertNoErr(t, SetChapters(ctx, fixturePath, dst, chapters))

	c, err := Open(ctx, dst)
	assertNoErr(t, err)
	if len(c.Chapters) != 2 {
		t.Fatalf("Chapters = %+v, want 2", c.Chapters)
	}
	if c.Chapters[0].Title != "Intro" || c.Chapters[1].Title != "Main" {
		t.Errorf("Chapters = %+v, want Intro/Main in order", c.Chapters)
	}
}

// TestFacadeOGMChaptersRoundTrip proves ParseOGMChapters and
// FormatOGMChapters round-trip through the facade.
func TestFacadeOGMChaptersRoundTrip(t *testing.T) {
	chapters := []Chapter{
		{StartMs: 0, Title: "Intro"},
		{StartMs: 61500, Title: "Main Event"},
	}

	var buf bytes.Buffer
	assertNoErr(t, FormatOGMChapters(&buf, chapters))
	if !strings.Contains(buf.String(), "CHAPTER01=00:00:00.000") {
		t.Errorf("formatted output missing expected timestamp:\n%s", buf.String())
	}

	parsed, err := ParseOGMChapters(strings.NewReader(buf.String()))
	assertNoErr(t, err)
	if len(parsed) != 2 {
		t.Fatalf("parsed = %+v, want 2 chapters", parsed)
	}
	if parsed[0].Title != "Intro" || parsed[0].StartMs != 0 {
		t.Errorf("parsed[0] = %+v", parsed[0])
	}
	if parsed[1].Title != "Main Event" || parsed[1].StartMs != 61500 {
		t.Errorf("parsed[1] = %+v", parsed[1])
	}
}

// TestFacadeTrackDefaultDurations proves the wrapper builds the track-number
// to DefaultDuration map, skipping tracks without a default duration.
func TestFacadeTrackDefaultDurations(t *testing.T) {
	tracks := []Track{
		{ID: 1, Type: VideoTrack, DefaultDurationNs: 41708333},
		{ID: 2, Type: AudioTrack}, // no default duration: must be excluded
	}
	durations := TrackDefaultDurations(tracks)
	if durations[1] != 41708333 {
		t.Errorf("durations[1] = %d, want 41708333", durations[1])
	}
	if _, ok := durations[2]; ok {
		t.Error("durations should not contain an entry for a track with no DefaultDuration")
	}
}
