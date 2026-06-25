package reader

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestDuplicateTracksFirstWins covers a non-conformant file with two Tracks
// elements: Read must take the FIRST set (not append a second, doubling the
// tracks), and Read and ReadMeta must agree.
func TestDuplicateTracksFirstWins(t *testing.T) {
	tracks1 := masterElem(mkv.IDTracks, trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_MPEGH/ISO/HEVC"),
	))
	tracks2 := masterElem(mkv.IDTracks, trackEntry(
		uintElem(mkv.IDTrackNumber, 2, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_AAC"),
	))
	file := segmentMKV(infoElem(), tracks1, tracks2, clusterElem())

	c, err := Read(context.Background(), bytes.NewReader(file), "x.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(c.Tracks) != 1 || c.Tracks[0].ID != 1 {
		t.Errorf("Read tracks = %d, want 1 (first set only); got %+v", len(c.Tracks), c.Tracks)
	}

	cm, err := ReadMeta(context.Background(), bytes.NewReader(file), "x.mkv")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if len(cm.Tracks) != len(c.Tracks) {
		t.Errorf("ReadMeta tracks = %d, Read tracks = %d — must agree", len(cm.Tracks), len(c.Tracks))
	}
}

// TestParseErrorHasOffsetAndID covers the debuggability fix: a parse failure
// carries the failing element's ID and byte offset, not a bare error.
func TestParseErrorHasOffsetAndID(t *testing.T) {
	// An Info with a Duration of size 3 (valid float sizes are 4/8) makes parseInfo
	// fail; the Segment walk must wrap it with the element id + offset.
	badDuration := append(append(idBytes(mkv.IDDuration), 0x83), 1, 2, 3)
	info := masterElem(mkv.IDInfo, badDuration)
	file := segmentMKV(info, tracksElem(), clusterElem())

	_, err := Read(context.Background(), bytes.NewReader(file), "x.mkv")
	if err == nil {
		t.Fatal("expected a parse error")
	}
	s := err.Error()
	for _, want := range []string{"0x1549A966", "offset", "invalid float size"} {
		if !strings.Contains(s, want) {
			t.Errorf("error %q missing %q", s, want)
		}
	}
}
