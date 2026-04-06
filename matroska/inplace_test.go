package matroska

import (
	"context"
	"os"
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
	dir := t.TempDir()
	path := dir + "/copy.mkv"
	copyFile(t, fixturePath, path)

	err := EditInPlace(context.Background(), path, func(c *Container) {
		// Set a very long title that won't fit in the available space
		longTitle := ""
		for i := 0; i < 1000; i++ {
			longTitle += "very long title padding "
		}
		c.Info.Title = longTitle
	})
	if err == nil {
		t.Fatal("expected error for metadata too large")
	}
	t.Logf("expected error: %v", err)

	// File should be unchanged
	c, _ := Open(context.Background(), path)
	assertEqual(t, c.Info.Title, "Test Fixture", "title unchanged after failed edit")
}
