package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mp4"
)

// TestAnalyzeSaysTheSourceIsCut: the stats of a source that stops before its
// declared end cover what arrived, and the report says so - for a Matroska
// cut between two clusters and for an MP4 cut in its media - instead of
// totals that read as the whole file's.
func TestAnalyzeSaysTheSourceIsCut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mkvData := readAll(t, salvageFixture(t, dir, "src.mkv", 8))
	// moov ahead of mdat, so a cut keeps the sample tables and loses media.
	faststart := filepath.Join(dir, "fs.mp4")
	if err := mp4.RemuxToMP4(ctx, filepath.Join("..", "..", "internal", "testdata", "sample.mkv"), faststart, mp4.Options{FastStart: true}); err != nil {
		t.Fatal(err)
	}
	mp4Data, err := os.ReadFile(faststart)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		data []byte
		cut  bool
	}{
		"mkv whole":                {mkvData, false},
		"mkv cut between clusters": {mkvData[:findAll(mkvData, []byte{0x1F, 0x43, 0xB6, 0x75})[5]], true},
		"mp4 whole":                {mp4Data, false},
		"mp4 cut in its media":     {mp4Data[:len(mp4Data)-100], true},
	} {
		path := filepath.Join(dir, "f")
		writeAll(t, path, c.data)
		r, err := Analyze(ctx, path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		said := false
		for _, w := range r.Warnings {
			said = said || strings.Contains(w, "stops before its declared end")
		}
		if said != c.cut {
			t.Errorf("%s: cut reported %v, want %v (warnings %q)", name, said, c.cut, r.Warnings)
		}
	}
}
