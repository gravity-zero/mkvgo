package mp4

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestManifestsQuoteFileStrings: a track's name and language come from the file. Written with Go's %q, a '"' in
// them closed the HLS quoted string (HLS has no escape) and the DASH attribute (%q is not XML escaping), so a file
// could add attributes to the master playlist and elements - a BaseURL to another host - to the manifest.
func TestManifestsQuoteFileStrings(t *testing.T) {
	ch := uint8(2)
	video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(640), Height: u32(360)}
	audio := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: &ch,
		Name: `a",GROUP-ID="x`, LanguageBCP47: `en"><BaseURL>https://evil.example/</BaseURL><X a="`}
	src := buildABRVariant(t, video, audio)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	master, err := os.ReadFile(filepath.Join(dir, "master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(master), "\n") {
		if strings.HasPrefix(line, "#EXT-X-MEDIA:") && (strings.Count(line, `"`)%2 != 0 || strings.Contains(line, `GROUP-ID="x"`) || strings.Contains(line, `\"`)) {
			t.Errorf("a track's name escaped its quoted string: %s", line)
		}
	}
	mpd, err := os.ReadFile(filepath.Join(dir, "manifest.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Sets []struct {
			Lang string   `xml:"lang,attr"`
			Base []string `xml:"BaseURL"`
		} `xml:"Period>AdaptationSet"`
	}
	if err := xml.Unmarshal(mpd, &doc); err != nil {
		t.Fatalf("the manifest is not XML: %v", err)
	}
	langs := 0
	for _, s := range doc.Sets {
		if len(s.Base) != 0 {
			t.Errorf("the manifest gained BaseURL %v from a track's language", s.Base)
		}
		if s.Lang == audio.LanguageBCP47 {
			langs++
		}
	}
	if langs != 1 {
		t.Errorf("%d AdaptationSets carry the language whole, want 1:\n%s", langs, mpd)
	}
}
