package commands

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadContainerRoutesMislabeledMOV covers a real corpus shape: a QuickTime
// file saved with a .mkv extension (mislabeled rips happen). The inspection
// commands must read it through the mp4 reader instead of dying on a cryptic
// EBML-header error, so loadContainer falls back on ErrNotMatroska.
func TestLoadContainerRoutesMislabeledMOV(t *testing.T) {
	src := filepath.Join("..", "..", "..", "internal", "testdata", "quicktime.mov")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read quicktime.mov: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "mislabeled.mkv")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write mislabeled copy: %v", err)
	}

	c, _ := loadContainer(dst, false)
	if len(c.Tracks) == 0 {
		t.Fatal("a MOV saved as .mkv produced no tracks - the mp4 fallback did not fire")
	}
}
