package reader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/internal/costguard"
	"github.com/gravity-zero/mkvgo/mkv"
)

// FuzzReadVsReadStream: on an input both readers accept, ReadStream and Read describe the same file - Info, Tracks,
// Chapters and editions, Attachments, Tags and the EBML DocType. What legitimately differs is normalized:
//   - ReadStream stops at the first Cluster, where Read goes on to Cues and metadata the SeekHead points past it,
//     or found by reading the file's end back, so Read is given the input cut before that Cluster's header;
//   - Cues, Keyframes, SegmentStart, ResyncedBytes and Path are the seekable reader's alone and are not compared;
//   - an input Read judges by looking past an element's end (an element read as junk, a parent whose size it takes
//     for wrong), which a stream cannot, or that the two resync differently, is not compared either: what each
//     reader recovers past such bytes is its own.
func FuzzReadVsReadStream(f *testing.F) {
	f.Add(buildMinimalMKV().Bytes())
	f.Add(liveCuesMKV())
	f.Add(describedMKV(f))
	for _, name := range []string{"sample.mkv", "meta.mkv", "av1.mkv", "vp9.webm"} {
		if b, err := os.ReadFile(filepath.Join("..", "..", "internal", "testdata", name)); err == nil {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		costguard.Fuzz(t, len(data), func() {
			cst, br, err := ReadStream(context.Background(), bytes.NewReader(data))
			if err != nil {
				return
			}
			cut := beforeCluster(data, min(int(br.r.tell()), len(data)), br.inCluster)
			var lookedPast bool
			cs, err := Read(context.Background(), bytes.NewReader(data[:cut]), "fuzz.mkv", withLookedPast(&lookedPast))
			if err != nil {
				if len(cst.Tracks) > 0 && !readOnlyElement(err) && !lookedPast {
					t.Errorf("Read refuses the %d bytes ReadStream described %d tracks from: %v", cut, len(cst.Tracks), err)
				}
				return
			}
			if lookedPast || cs.ResyncedBytes != cst.ResyncedBytes {
				return
			}
			if got, want := comparable(cst), comparable(cs); got != want {
				t.Errorf("ReadStream and Read differ on %d bytes (cut at %d):\nstream: %s\nread:   %s", len(data), cut, got, want)
			}
		})
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

// beforeCluster backs cut over the Cluster header ReadStream stopped after, when it entered one.
func beforeCluster(data []byte, cut int, inCluster bool) int {
	if !inCluster {
		return cut
	}
	for n := 5; n <= 12 && n <= cut; n++ {
		h, hl, err := ebml.ReadElementHeader(bytes.NewReader(data[cut-n : cut]))
		if err == nil && h.ID == mkv.IDCluster && hl == n {
			return cut - n
		}
	}
	return cut
}

// readOnlyElement tells an error Read met in an element ReadStream passes over unread - the Cues, a SeekHead - from
// one in an element both decode.
func readOnlyElement(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, fmt.Sprintf("element 0x%X ", mkv.IDCues)) || strings.Contains(msg, fmt.Sprintf("element 0x%X ", mkv.IDSeekHead))
}
