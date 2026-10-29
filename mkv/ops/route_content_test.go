package ops

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestRoutesByContent: Playability and Fingerprint took the MP4 reader on a .mp4/.m4v/.mov/.m4a name, so a Matroska
// file under such a name failed in the MP4 reader. The first bytes choose the reader, whatever the name says.
func TestRoutesByContent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, c := range []struct{ src, name string }{
		{"gop2s_aac.mkv", "really-mkv.mp4"},
		{"gop2s_aac.mkv", "really-mkv.mov"},
		{"bframes_aac.mp4", "really-mp4.mkv"},
	} {
		data, err := os.ReadFile(filepath.Join("..", "..", "internal", "testdata", c.src))
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, c.name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Playability(ctx, p, Target{}); err != nil {
			t.Errorf("Playability(%s, %s inside): %v", c.name, c.src, err)
		}
		if _, err := Fingerprint(ctx, p); err != nil {
			t.Errorf("Fingerprint(%s, %s inside): %v", c.name, c.src, err)
		}
	}
}
