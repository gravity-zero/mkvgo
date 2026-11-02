package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestSourceCutShort: a source that stops before the end it declares - a
// Matroska Segment, the top-level MP4 boxes - is reported with both sizes,
// by path and over an open reader; a whole file, trailing junk and a
// Segment of unknown size are not.
func TestSourceCutShort(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mkvData := readAll(t, salvageFixture(t, dir, "src.mkv", 8))
	mp4Data, err := os.ReadFile(filepath.Join("..", "..", "internal", "testdata", "fps30.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	streamed := append([]byte{}, mkvData...)
	seg := bytes.Index(streamed, []byte{0x18, 0x53, 0x80, 0x67})
	copy(streamed[seg+4:], []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) // unknown size, 8-byte VINT
	for name, c := range map[string]struct {
		data     []byte
		declared int64 // 0: not cut short
	}{
		"mkv whole":         {mkvData, 0},
		"mkv cut":           {mkvData[:len(mkvData)-100], int64(len(mkvData))},
		"mkv trailing junk": {append(append([]byte{}, mkvData...), make([]byte, 64)...), 0},
		"mkv unknown size":  {streamed[:len(streamed)-100], 0},
		"mp4 whole":         {mp4Data, 0},
		"mp4 cut":           {mp4Data[:len(mp4Data)-100], int64(len(mp4Data))},
	} {
		path := filepath.Join(dir, "f")
		writeAll(t, path, c.data)
		byPath, err := SourceCutShort(ctx, path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		byReader, err := CutShortOf(ctx, bytes.NewReader(c.data))
		if err != nil {
			t.Fatalf("%s (reader): %v", name, err)
		}
		for via, got := range map[string]*CutShort{"path": byPath, "reader": byReader} {
			switch {
			case c.declared == 0 && got != nil:
				t.Errorf("%s (%s): reported cut short: %+v", name, via, *got)
			case c.declared != 0 && (got == nil || got.FileSize != int64(len(c.data)) || got.DeclaredSize != c.declared):
				t.Errorf("%s (%s): got %+v, want %d of %d", name, via, got, len(c.data), c.declared)
			}
		}
	}
}
