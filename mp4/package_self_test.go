package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestPackagingNeverWritesOverASource pins that a source lying in the output directory under a name the packager
// writes is refused before anything is written, while one under its own name packages as usual.
func TestPackagingNeverWritesOverASource(t *testing.T) {
	ctx := context.Background()
	data, err := os.ReadFile(editShiftFixture(t, false))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	clash := filepath.Join(dir, "init.mp4")
	if err := os.WriteFile(clash, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemuxToHLS(ctx, clash, dir); err == nil {
		t.Error("packaging a source named init.mp4 into its own directory succeeded")
	}
	if got, err := os.ReadFile(clash); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the source named init.mp4 was changed or removed (%v)", err)
	}
	own := filepath.Join(dir, "movie.mp4")
	if err := os.WriteFile(own, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemuxToHLS(ctx, own, dir); err != nil {
		t.Errorf("a source under its own name next to the output was refused: %v", err)
	}
}
