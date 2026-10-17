package reader

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// FuzzReadVsReadStream: on an input both readers accept, ReadStream and Read describe the same file - Info, Tracks,
// Chapters and editions, Attachments, Tags and the EBML DocType. What legitimately differs is normalized:
//   - ReadStream stops at the first Cluster, where Read goes on to Cues and metadata the SeekHead points past it,
//     so Read is given the input cut where ReadStream stopped;
//   - Cues, Keyframes, SegmentStart, ResyncedBytes and Path are the seekable reader's alone and are not compared.
func FuzzReadVsReadStream(f *testing.F) {
	f.Add(buildMinimalMKV().Bytes())
	f.Add(liveCuesMKV())
	for _, name := range []string{"sample.mkv", "meta.mkv", "av1.mkv", "vp9.webm"} {
		if b, err := os.ReadFile(filepath.Join("..", "..", "internal", "testdata", name)); err == nil {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		cst, br, err := ReadStream(context.Background(), bytes.NewReader(data))
		if err != nil {
			return
		}
		cut := min(int(br.r.tell()), len(data))
		cs, err := Read(context.Background(), bytes.NewReader(data[:cut]), "fuzz.mkv")
		if err != nil {
			return
		}
		if got, want := comparable(cst), comparable(cs); got != want {
			t.Errorf("ReadStream and Read differ on %d bytes (cut at %d):\nstream: %s\nread:   %s", len(data), cut, got, want)
		}
	})
}

// comparable renders what both readers must agree on, pointed-to values included.
func comparable(c *mkv.Container) string {
	b, err := json.Marshal(mkv.Container{Info: c.Info, Tracks: c.Tracks, Chapters: c.Chapters, OtherEditions: c.OtherEditions,
		Attachments: c.Attachments, Tags: c.Tags, DocType: c.DocType, DocTypeVersion: c.DocTypeVersion,
		DocTypeReadVersion: c.DocTypeReadVersion, DurationMs: c.DurationMs})
	if err != nil {
		return err.Error()
	}
	return string(b)
}
