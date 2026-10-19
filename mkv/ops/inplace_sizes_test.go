package ops

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestParseSeekHeadBodyBoundsADeclaredSize: a Seek entry declaring 2^56 bytes inside a SeekHead of a few bytes ends
// the parse, where it was allocated and panicked.
func TestParseSeekHeadBodyBoundsADeclaredSize(t *testing.T) {
	body := []byte{0x4D, 0xBB, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFE, 0x53, 0xAB}
	if got := parseSeekHeadBody(body); len(got) != 0 {
		t.Errorf("parsed %d entries from a Seek past its SeekHead", len(got))
	}
}

// TestEditInPlaceRefusesAHeadRunningOverTheClusters: a head Void whose size runs over the first cluster would have
// the edit zero that span with its own Void; the edit is refused and the file left as it was.
func TestEditInPlaceRefusesAHeadRunningOverTheClusters(t *testing.T) {
	dir := t.TempDir()
	var blocks []mkv.Block
	for tc := int64(0); tc <= 2000; tc += 100 {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: make([]byte, 500)})
	}
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, blocks, 2100)
	out := filepath.Join(dir, "muxed.mkv")
	mustNil(t, Mux(context.Background(), mkv.MuxOptions{OutputPath: out, Tracks: []mkv.TrackInput{{SourcePath: src, TrackID: 1}}}))
	data, err := os.ReadFile(out)
	mustNil(t, err)

	r := bytes.NewReader(data)
	h, _, err := ebml.ReadElementHeader(r)
	mustNil(t, err)
	_, _ = r.Seek(h.Size, 1)
	_, _, err = ebml.ReadElementHeader(r) // Segment
	mustNil(t, err)
	for {
		pos := int(r.Size()) - r.Len()
		eh, n, err := ebml.ReadElementHeader(r)
		mustNil(t, err)
		if eh.ID == mkv.IDCluster {
			t.Fatal("no Void before the first cluster")
		}
		if eh.ID == mkv.IDVoid {
			idLen := 1
			width := n - idLen
			if width < 2 {
				t.Fatalf("a %d-byte Void size field cannot reach the clusters", width)
			}
			var buf [8]byte // the widest size the field holds, its length marker set
			binary.BigEndian.PutUint64(buf[:], uint64(1)<<(7*width)|(uint64(1)<<(7*width)-2))
			copy(data[pos+idLen:], buf[8-width:])
			break
		}
		_, _ = r.Seek(eh.Size, 1)
	}
	mustNil(t, os.WriteFile(out, data, 0o644))
	err = EditInPlace(context.Background(), out, func(c *mkv.Container) { c.Info.Title = "edited" })
	if err == nil {
		t.Error("an edit over a head Void reaching into the clusters succeeded")
	}
	after, _ := os.ReadFile(out)
	if !bytes.Equal(after, data) {
		t.Error("the file was changed")
	}
}

// TestReindexInPlaceRefusesBytesPastTheSegment: a file with another appended after its Segment would have the
// appended bytes swallowed into the extended Segment; the in-place reindex is refused and the file left as it was.
func TestReindexInPlaceRefusesBytesPastTheSegment(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.mkv"), filepath.Join(dir, "b.mkv")
	sparseTracksMKV(t, a, []mkv.Track{videoTrack(1)})
	sparseTracksMKV(t, b, []mkv.Track{videoTrack(1)})
	da, _ := os.ReadFile(a)
	db, _ := os.ReadFile(b)
	cat := append(append([]byte(nil), da...), db...)
	mustNil(t, os.WriteFile(a, cat, 0o644))
	if err := ReindexInPlace(context.Background(), a); err == nil || !strings.Contains(err.Error(), "mkvgo reindex") {
		t.Errorf("in-place reindex of a file with bytes past its Segment: %v", err)
	}
	if after, _ := os.ReadFile(a); !bytes.Equal(after, cat) {
		t.Error("the file was changed")
	}
}
