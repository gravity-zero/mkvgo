package mp4

import (
	"bytes"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestWebVTTAdditionsInCMAF: a Matroska WebVTT cue's BlockAddition - its settings, then its identifier - reaches the
// HLS WebVTT cue's settings and the native wvtt sample's sttg and iden boxes.
func TestWebVTTAdditionsInCMAF(t *testing.T) {
	adds := []mkv.BlockAddition{{ID: 1, Data: []byte("line:90% align:start\nintro\nNOTE x\n")}}
	cue, ok, err := subCueFromBlock(&mkv.Track{Codec: "webvtt"}, mkv.Block{Timecode: 1000, Duration: 500, Data: []byte("Hello"), Additions: adds})
	if err != nil || !ok || cue.Settings != "line:90% align:start" {
		t.Fatalf("HLS cue %+v, %v, %v; want its settings", cue, ok, err)
	}
	s := encodeWVTTCueWith([]byte("Hello"), adds)
	for _, want := range [][]byte{
		append([]byte{0, 0, 0, 13}, "idenintro"...),
		append([]byte{0, 0, 0, 28}, "sttgline:90% align:start"...),
		append([]byte{0, 0, 0, 13}, "paylHello"...),
	} {
		if !bytes.Contains(s, want) {
			t.Errorf("wvtt sample % x lacks %q", s, want[4:])
		}
	}
	if !bytes.Equal(encodeWVTTCueWith([]byte("Hello"), nil), encodeWVTTCue([]byte("Hello"))) {
		t.Error("a cue without additions changed")
	}
}
