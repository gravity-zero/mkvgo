package mp4

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestDashFrameRate pins the MPD rational frame-rate formatting: the NTSC
// rationals, the integer form, and the generic fractional fallback. Mutation
// testing showed the branches in dashFrameRate survived unkilled.
func TestDashFrameRate(t *testing.T) {
	cases := []struct {
		fps  float64
		want string
	}{
		{23.976, "24000/1001"},
		{29.97, "30000/1001"},
		{59.94, "60000/1001"},
		{30, "30"},
		{25, "25"},
		{24, "24"},
		{23.5, "23500/1000"}, // no NTSC match, non-integer -> generic /1000
	}
	for _, c := range cases {
		if got := dashFrameRate(c.fps); got != c.want {
			t.Errorf("dashFrameRate(%v) = %q, want %q", c.fps, got, c.want)
		}
	}
}

// TestRemuxToHLS_DASHAttributes covers buildDASHManifest (dash.go) - the
// single-variant DASH RemuxToHLS emits, a separate code path from combinedDASH
// (abr.go). It asserts each optional Representation/AdaptationSet attribute is
// present with full metadata and absent with minimal metadata, plus that the
// SegmentTimeline run-length-compresses equal-duration segments (the r="..."
// attribute).
func TestRemuxToHLS_DASHAttributes(t *testing.T) {
	ctx := context.Background()
	sr := 48000.0
	fr := 30.0
	ch := uint8(2)

	t.Run("full-metadata", func(t *testing.T) {
		video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(1280), Height: u32(720), FrameRate: &fr}
		audio := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "deu"}
		src := buildABRVariant(t, video, audio)
		dir := t.TempDir()
		if err := RemuxToHLS(ctx, src, dir, Options{SegmentMs: 1000}); err != nil {
			t.Fatal(err)
		}
		mpd := readTextFile(t, filepath.Join(dir, "manifest.mpd"))
		for _, want := range []string{`width="1280"`, `height="720"`, `frameRate=`, `lang="deu"`, `audioSamplingRate="48000"`} {
			mustContain(t, mpd, want)
		}
		// Constant-duration segments must run-length compress: at least one <S>
		// carries a repeat count.
		if !strings.Contains(mpd, ` r="`) {
			t.Errorf("SegmentTimeline did not run-length compress equal-duration segments (no r= attribute):\n%s", mpd)
		}
	})

	t.Run("minimal-metadata", func(t *testing.T) {
		video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(320), Height: u32(240)}
		audio := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: &ch}
		src := buildABRVariant(t, video, audio)
		dir := t.TempDir()
		if err := RemuxToHLS(ctx, src, dir, Options{SegmentMs: 1000}); err != nil {
			t.Fatal(err)
		}
		mpd := readTextFile(t, filepath.Join(dir, "manifest.mpd"))
		for _, notWant := range []string{`frameRate=`, ` lang=`, `audioSamplingRate=`} {
			mustNotContain(t, mpd, notWant)
		}
	})
}
