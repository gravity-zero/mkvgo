package matroska_test

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mp4"
)

// mkvgo's outputs are deterministic: no wall-clock timestamps, no random IDs,
// no map-ordered elements - the same input and options produce byte-identical
// files. This is a guarantee (content-addressed storage, dedup and golden
// tests rely on it), so a run that stops being reproducible is a regression.
func TestDeterministicOutputs(t *testing.T) {
	src, err := os.ReadFile("../internal/testdata/sample.mkv")
	if err != nil {
		t.Fatal(err)
	}

	run := func() (mkvOut, mp4Out, hlsInit, hlsSeg []byte) {
		m := mkv.NewMemFS()
		m.Put("in.mkv", src)
		err := matroska.EditMetadata(context.Background(), "in.mkv", "out.mkv",
			func(c *matroska.Container) { c.Info.Title = "Determinism" },
			matroska.Options{FS: m.FS()})
		if err != nil {
			t.Fatal(err)
		}
		if err := mp4.RemuxToMP4(context.Background(), "in.mkv", "out.mp4",
			mp4.Options{FS: m.FS(), FastStart: true}); err != nil {
			t.Fatal(err)
		}
		if err := mp4.RemuxToHLS(context.Background(), "in.mkv", "hls",
			mp4.Options{FS: m.FS(), SegmentMs: 500}); err != nil {
			t.Fatal(err)
		}
		return m.Get("out.mkv"), m.Get("out.mp4"), m.Get("hls/init.mp4"), m.Get("hls/seg00001.m4s")
	}

	aMKV, aMP4, aInit, aSeg := run()
	bMKV, bMP4, bInit, bSeg := run()
	for _, c := range []struct {
		name string
		a, b []byte
	}{
		{"MKV rewrite", aMKV, bMKV},
		{"MP4 remux", aMP4, bMP4},
		{"HLS init segment", aInit, bInit},
		{"HLS media segment", aSeg, bSeg},
	} {
		if len(c.a) == 0 {
			t.Errorf("%s: empty output", c.name)
			continue
		}
		if !bytes.Equal(c.a, c.b) {
			t.Errorf("%s: two identical runs produced different bytes (%d vs %d)", c.name, len(c.a), len(c.b))
		}
	}
}
