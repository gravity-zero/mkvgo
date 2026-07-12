package commands_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mp4"
)

// TestRemuxMP4RoundTrip exercises the remux used by the to-mp4/from-mp4 commands
// on the real muxer-written fixture (H.264 + 2× AAC + 2 chapters): MKV → MP4 →
// MKV, checking the tracks and chapters survive.
func TestRemuxMP4RoundTrip(t *testing.T) {
	dir := t.TempDir()
	mp4Path := filepath.Join(dir, "out.mp4")

	if err := mp4.RemuxToMP4(context.Background(), regfixMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	if fi, err := os.Stat(mp4Path); err != nil || fi.Size() == 0 {
		t.Fatalf("mp4 output missing or empty: %v", err)
	}

	backPath := filepath.Join(dir, "back.mkv")
	if err := mp4.RemuxFromMP4(context.Background(), mp4Path, backPath); err != nil {
		t.Fatalf("RemuxFromMP4: %v", err)
	}

	c, err := reader.Open(context.Background(), backPath)
	if err != nil {
		t.Fatalf("open round-tripped mkv: %v", err)
	}
	if len(c.Tracks) != 3 {
		t.Errorf("got %d tracks, want 3 (h264 + 2× aac)", len(c.Tracks))
	}
	if len(c.Chapters) != 2 {
		t.Errorf("got %d chapters, want 2", len(c.Chapters))
	}
}
