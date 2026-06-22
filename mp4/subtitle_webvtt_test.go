package mp4

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func TestExtractSubtitleWebVTTMP4(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(640), Height: u32p(480), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt"},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 1000, key: true, data: []byte("First")},
		{track: 1, pts: 40, key: false, data: []byte{2}},
		{track: 2, pts: 3000, key: true, data: []byte("<i>Second</i>")},
	}
	srcMKV := buildMKVWithChapters(t, tracks, blocks, nil)
	mp4Path := filepath.Join(t.TempDir(), "sub.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}

	// The subtitle track is the second track (mv.tracks order) → ID 2.
	var b strings.Builder
	if err := ExtractSubtitleWebVTT(context.Background(), mp4Path, 2, &b); err != nil {
		t.Fatalf("ExtractSubtitleWebVTT: %v", err)
	}
	got := b.String()
	if !strings.HasPrefix(got, "WEBVTT") {
		t.Errorf("output is not WebVTT:\n%s", got)
	}
	// tx3g carries plain text (markup stripped at remux); cue 1 spans to cue 2.
	for _, want := range []string{"00:00:01.000 --> 00:00:03.000\nFirst", "Second"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestExtractSubtitleWebVTTMP4_NotSubtitle(t *testing.T) {
	mp4Path := buildTestMP4(t) // video + audio, no subtitle
	var b strings.Builder
	if err := ExtractSubtitleWebVTT(context.Background(), mp4Path, 1, &b); err == nil {
		t.Error("expected error: track 1 is video, not a subtitle track")
	}
}
