package mp4

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestRemuxToMP4WritesSelectionMetadata checks the write side preserves the
// track-selection metadata the read side now reports: the default flag (tkhd
// track_enabled), the forced flag (DASH-role kind box) and the BCP-47 language
// (elng box) all round-trip MKV → MP4 → OpenMeta.
func TestRemuxToMP4WritesSelectionMetadata(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25),
			IsDefault: true, Language: "eng"},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC,
			Channels: u8p(2), SampleRate: f64p(48000),
			IsDefault: false, IsForced: true, Language: "fre", LanguageBCP47: "fr-FR"},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 0, key: true, data: []byte{2}},
	}
	srcMKV := buildMKVWithChapters(t, tracks, blocks, nil)
	mp4Path := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}

	c, _, err := OpenMeta(context.Background(), mp4Path)
	if err != nil {
		t.Fatalf("OpenMeta: %v", err)
	}
	var video, audio *mkv.Track
	for i := range c.Tracks {
		switch c.Tracks[i].Type {
		case mkv.VideoTrack:
			video = &c.Tracks[i]
		case mkv.AudioTrack:
			audio = &c.Tracks[i]
		}
	}
	if video == nil || audio == nil {
		t.Fatalf("missing track: video=%v audio=%v", video != nil, audio != nil)
	}

	if !video.IsDefault {
		t.Error("video should be default (enabled track)")
	}
	if audio.IsDefault {
		t.Error("audio should NOT be default (it was FlagDefault=0)")
	}
	if !audio.IsForced {
		t.Error("audio forced flag lost (kind box not written/read)")
	}
	if video.IsForced {
		t.Error("video must not be forced")
	}
	if audio.LanguageBCP47 != "fr-FR" {
		t.Errorf("audio LanguageBCP47 = %q, want fr-FR (elng round-trip)", audio.LanguageBCP47)
	}
	if audio.Language != "fre" {
		t.Errorf("audio Language = %q, want fre", audio.Language)
	}
}
