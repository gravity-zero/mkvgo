package matroska

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestEditInPlaceTitle(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	path := dir + "/copy.mkv"
	copyFile(t, fixturePath, path)

	origInfo, _ := os.Stat(path)

	assertNoErr(t, EditInPlace(context.Background(), path, func(c *Container) {
		c.Info.Title = "Short"
	}))

	c, err := Open(context.Background(), path)
	assertNoErr(t, err)
	assertEqual(t, c.Info.Title, "Short", "title")

	// File size should be unchanged (Void padding absorbed the difference)
	newInfo, _ := os.Stat(path)
	assertEqual(t, newInfo.Size(), origInfo.Size(), "file size")

	// Blocks should still be intact
	counts := countBlocks(t, path, c.Info.TimecodeScale)
	if counts[1] == 0 || counts[2] == 0 {
		t.Errorf("blocks broken: %v", counts)
	}
}

func TestEditInPlaceTrackLanguage(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	path := dir + "/copy.mkv"
	copyFile(t, fixturePath, path)

	assertNoErr(t, EditInPlace(context.Background(), path, func(c *Container) {
		c.Tracks[1].Language = "fre"
	}))

	c, err := Open(context.Background(), path)
	assertNoErr(t, err)
	assertEqual(t, c.Tracks[1].Language, "fre", "language")
}

func TestEditInPlaceTooLarge(t *testing.T) {
	requireFixture(t)
	longTitle := strings.Repeat("very long title padding ", 1000)
	edit := func(c *Container) { c.Info.Title = longTitle }

	// The head cannot hold it: the Info moves to the end of the Segment.
	path := t.TempDir() + "/copy.mkv"
	copyFile(t, fixturePath, path)
	assertNoErr(t, EditInPlace(context.Background(), path, edit))
	c, err := Open(context.Background(), path)
	assertNoErr(t, err)
	assertEqual(t, c.Info.Title, longTitle, "title moved to the end of the Segment")

	// Bytes after the Segment leave nowhere to move it: refused, the file unchanged.
	path = t.TempDir() + "/trailing.mkv"
	copyFile(t, fixturePath, path)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	assertNoErr(t, err)
	_, err = f.Write([]byte{0xEC, 0x81, 0x00}) // a 3-byte Void
	assertNoErr(t, f.Close())
	assertNoErr(t, err)
	if err := EditInPlace(context.Background(), path, edit); !errors.Is(err, ErrNoRoomInPlace) {
		t.Fatalf("err = %v, want ErrNoRoomInPlace", err)
	}
	c, _ = Open(context.Background(), path)
	assertEqual(t, c.Info.Title, "Test Fixture", "title unchanged after failed edit")
}
