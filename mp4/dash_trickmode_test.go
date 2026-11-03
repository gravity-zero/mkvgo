package mp4

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestDASHTrickMode: the DASH manifest of a presentation with I-frame fragments carries a DASH-IF trick-mode
// AdaptationSet over them - the video set named by id, one SegmentURL per fragment iframe.m3u8 lists, each a file
// to-hls wrote - and an encrypted one, whose I-frame fragments are not written, none.
func TestDASHTrickMode(t *testing.T) {
	sr := 48000.0
	two := uint8(2)
	fps := 25.0
	video := manifestVideo()
	video.FrameRate = &fps
	var blocks []genBlock
	for i := range 250 {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: cencVideoSample()})
	}
	for i := range 300 {
		blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 32, key: true, data: cencAudioSample(i)})
	}
	sortGenBlocks(blocks)
	src := buildMKV(t, []mkv.Track{video, {ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: []byte{0x11, 0x90}, SampleRate: &sr, Channels: &two}}, blocks)
	dir := filepath.Join(t.TempDir(), "hls")
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	mpd := readTextFile(t, filepath.Join(dir, "manifest.mpd"))
	for _, want := range []string{
		`<AdaptationSet id="1" mimeType="video/mp4" contentType="video">`,
		`<EssentialProperty schemeIdUri="http://dashif.org/guidelines/trickmode" value="1"/>`,
		`codingDependency="false"`, `maxPlayoutRate="50"`, `<Initialization sourceURL="init.mp4"/>`,
	} {
		if !strings.Contains(mpd, want) {
			t.Errorf("manifest.mpd lacks %s:\n%s", want, mpd)
		}
	}
	urls := regexp.MustCompile(`<SegmentURL media="([^"]+)"/>`).FindAllStringSubmatch(mpd, -1)
	listed := strings.Count(readTextFile(t, filepath.Join(dir, "iframe.m3u8")), "iframe0")
	if len(urls) == 0 || len(urls) != listed {
		t.Fatalf("%d trick-mode segments, iframe.m3u8 lists %d", len(urls), listed)
	}
	for _, u := range urls {
		if _, err := os.Stat(filepath.Join(dir, u[1])); err != nil {
			t.Errorf("trick-mode segment %s: %v", u[1], err)
		}
	}

	// The plan of the same source lists the same fragments, each served on demand.
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	pm, _, err := plan.Resource(context.Background(), "manifest.mpd")
	if err != nil {
		t.Fatal(err)
	}
	purls := regexp.MustCompile(`<SegmentURL media="([^"]+)"/>`).FindAllStringSubmatch(string(pm), -1)
	if len(purls) != len(urls) {
		t.Fatalf("plan lists %d trick-mode segments, the full pass %d", len(purls), len(urls))
	}
	for _, u := range purls {
		if _, _, err := plan.Resource(context.Background(), u[1]); err != nil {
			t.Errorf("plan serves no %s: %v", u[1], err)
		}
	}

	enc := filepath.Join(t.TempDir(), "enc")
	o := Options{SegmentMs: 2000, CENC: &CENCOptions{Scheme: "cenc", InlineKey: true, KeyID: make([]byte, 16), Key: make([]byte, 16), IV: make([]byte, 8)}}
	if err := RemuxToHLS(context.Background(), src, enc, o); err != nil {
		t.Fatal(err)
	}
	if mpd := readTextFile(t, filepath.Join(enc, "manifest.mpd")); strings.Contains(mpd, "trickmode") {
		t.Errorf("an encrypted presentation lists I-frame fragments it did not write:\n%s", mpd)
	}
}
