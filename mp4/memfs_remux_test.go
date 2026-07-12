package mp4

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// A full remux over the in-memory FS - no disk access at all. This is the
// exact path the WebAssembly build uses (browsers have no filesystem).
func TestRemuxToMP4OverMemFS(t *testing.T) {
	src, err := os.ReadFile("../internal/testdata/sample.mkv")
	if err != nil {
		t.Fatal(err)
	}
	m := mkv.NewMemFS()
	m.Put("in.mkv", src)

	if err := RemuxToMP4(context.Background(), "in.mkv", "out.mp4", Options{FS: m.FS()}); err != nil {
		t.Fatal(err)
	}
	out := m.Get("out.mp4")
	if len(out) == 0 || !bytes.Contains(out[:64], []byte("ftyp")) {
		t.Fatalf("in-memory MP4 output invalid (%d bytes)", len(out))
	}

	// And HLS: multiple output files land in the same in-memory FS.
	if err := RemuxToHLS(context.Background(), "in.mkv", "hls", Options{FS: m.FS(), SegmentMs: 500}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hls/master.m3u8", "hls/init.mp4", "hls/playlist.m3u8", "hls/seg00001.m4s"} {
		if len(m.Get(want)) == 0 {
			t.Errorf("HLS output %s missing (have: %v)", want, m.Paths())
		}
	}
}
