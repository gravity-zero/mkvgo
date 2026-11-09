package mp4

import (
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// An encrypted text subtitle is no WebVTT rendition: refused by name, or dropped with its reason under
// SkipUnsupported, as towards MP4. The HLS producers listed it and built its cues from the undecrypted bytes.
func TestHLSCarriesNoEncryptedSubtitle(t *testing.T) {
	c := &mkv.Container{Tracks: []mkv.Track{{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt", Encryption: "AES-CTR"}}}
	if subs, err := planSubTracks(c, Options{}); err == nil || !strings.Contains(err.Error(), "track 3 (srt) is encrypted") {
		t.Errorf("%d renditions, err = %v; want the track refused as encrypted", len(subs), err)
	}
	var dropped []DroppedTrack
	subs, err := planSubTracks(c, Options{SkipUnsupported: true, OnDrop: func(d DroppedTrack) { dropped = append(dropped, d) }})
	if err != nil || len(subs) != 0 || len(dropped) != 1 || dropped[0].ID != 3 {
		t.Errorf("under SkipUnsupported: %d renditions, dropped %+v, err %v; want none, track 3 dropped", len(subs), dropped, err)
	}
	if _, err := planSubTracks(c, Options{SkipUnsupported: true, Strict: true}); err == nil {
		t.Error("Strict let an encrypted subtitle be left out")
	}
}
