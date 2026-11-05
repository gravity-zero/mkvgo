package mp4

import (
	"context"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestDASHTrickModePlanMatchesFullPass: a stream whose first frames precede its first keyframe has a segment that
// opens on no keyframe. The full pass then lists no trick-mode set; the plan listed one fragment a segment and served
// no fragment for that segment. Every SegmentURL the plan's MPD names is served, its trick-mode set the full pass's.
func TestDASHTrickModePlanMatchesFullPass(t *testing.T) {
	sr := 48000.0
	two := uint8(2)
	fps := 25.0
	video := manifestVideo()
	video.FrameRate = &fps
	var blocks []genBlock
	for i := range 250 {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 10, data: cencVideoSample()})
	}
	for i := range 300 {
		blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 32, key: true, data: cencAudioSample(i)})
	}
	sortGenBlocks(blocks)
	src := buildMKV(t, []mkv.Track{video, {ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: []byte{0x11, 0x90}, SampleRate: &sr, Channels: &two}}, blocks)
	o := Options{SegmentMs: 2000}
	dir := filepath.Join(t.TempDir(), "hls")
	if err := RemuxToHLS(context.Background(), src, dir, o); err != nil {
		t.Fatal(err)
	}
	full := readTextFile(t, filepath.Join(dir, "manifest.mpd"))
	plan, err := PlanHLS(context.Background(), src, o)
	if err != nil {
		t.Fatal(err)
	}
	pm, _, err := plan.Resource(context.Background(), "manifest.mpd")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range regexp.MustCompile(`<SegmentURL media="(iframe[^"]+)"/>`).FindAllStringSubmatch(string(pm), -1) {
		if _, _, err := plan.Resource(context.Background(), u[1]); err != nil {
			t.Errorf("the plan's MPD names %s, which it does not serve: %v", u[1], err)
		}
	}
	iframeURLs := regexp.MustCompile(`<SegmentURL media="(iframe[^"]+)"/>`)
	if p, f := len(iframeURLs.FindAllString(string(pm), -1)), len(iframeURLs.FindAllString(full, -1)); p != f {
		t.Errorf("the plan's MPD lists %d trick-mode fragments, the full pass's %d", p, f)
	}
}
