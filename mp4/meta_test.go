package mp4

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// buildTestMP4 builds a small but representative MP4 (H.264 + AAC, colour code
// points, two chapters) and returns its path.
func buildTestMP4(t *testing.T) string {
	t.Helper()
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(1920), Height: u32p(1080), FrameRate: f64p(25), Language: "fre", IsDefault: true,
			ColorPrimaries: u16p(9), ColorTransfer: u16p(16), ColorSpace: u16p(9), ColorRange: u16p(1)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC,
			Channels: u8p(2), SampleRate: f64p(48000), Language: "fre", IsDefault: true},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 0, key: true, data: []byte{2}},
		{track: 1, pts: 40, key: false, data: []byte{3}},
		{track: 2, pts: 1000, key: true, data: []byte{4}},
	}
	chapters := []mkv.Chapter{{StartMs: 0, Title: "Alpha"}, {StartMs: 1000, Title: "Bravo"}}
	srcMKV := buildMKVWithChapters(t, tracks, blocks, chapters)

	mp4Path := filepath.Join(t.TempDir(), "in.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	return mp4Path
}

// TestOpenMetaMatchesRemux checks the metadata-only probe reports the same
// tracks, colour, chapters and duration that a full RemuxFromMP4 would, without
// writing any output file.
func TestOpenMetaMatchesRemux(t *testing.T) {
	mp4Path := buildTestMP4(t)

	c, dropped, err := OpenMeta(context.Background(), mp4Path, Options{Keyframes: true})
	if err != nil {
		t.Fatalf("OpenMeta: %v", err)
	}
	// The synthetic QuickTime chapter track (chapters are read from chpl) reads
	// back as a dropped "text" entry; no audio/video track must ever be dropped.
	for _, d := range dropped {
		if d.Type == mkv.VideoTrack || d.Type == mkv.AudioTrack {
			t.Errorf("a media track was dropped: %+v", d)
		}
	}

	if c.Path != mp4Path {
		t.Errorf("Path = %q, want %q", c.Path, mp4Path)
	}
	if len(c.Tracks) != 2 {
		t.Fatalf("got %d tracks, want 2", len(c.Tracks))
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

	if video.Codec != "h264" {
		t.Errorf("video codec = %q, want h264", video.Codec)
	}
	if video.Width == nil || *video.Width != 1920 || video.Height == nil || *video.Height != 1080 {
		t.Errorf("video dimensions = %v x %v, want 1920x1080", video.Width, video.Height)
	}
	if video.ColorPrimaries == nil || *video.ColorPrimaries != 9 ||
		video.ColorTransfer == nil || *video.ColorTransfer != 16 ||
		video.ColorSpace == nil || *video.ColorSpace != 9 {
		t.Errorf("colour code points not reported: primaries=%v transfer=%v matrix=%v",
			video.ColorPrimaries, video.ColorTransfer, video.ColorSpace)
	}
	if audio.Codec != "aac" {
		t.Errorf("audio codec = %q, want aac", audio.Codec)
	}

	// Language and the default flag must be read from the ISO-BMFF boxes (mdhd /
	// tkhd), not left at their zero values — they drive track selection.
	for _, tk := range []*mkv.Track{video, audio} {
		if !tk.LanguagePresent || tk.Language != "fre" {
			t.Errorf("%s language = %q (present=%v), want fre", tk.Type, tk.Language, tk.LanguagePresent)
		}
		if !tk.DefaultPresent || !tk.IsDefault {
			t.Errorf("%s default = %v (present=%v), want true (enabled track)", tk.Type, tk.IsDefault, tk.DefaultPresent)
		}
	}

	if len(c.Chapters) != 2 || c.Chapters[0].Title != "Alpha" || c.Chapters[1].Title != "Bravo" {
		t.Errorf("chapters not reported: %+v", c.Chapters)
	}
	if c.DurationMs <= 0 {
		t.Errorf("DurationMs = %d, want > 0", c.DurationMs)
	}

	// Attachments/Tags/Cues are never populated by the metadata path.
	if c.Attachments != nil || c.Tags != nil || c.Cues != nil {
		t.Errorf("metadata path must leave Attachments/Tags/Cues nil, got %v/%v/%v",
			c.Attachments, c.Tags, c.Cues)
	}

	// The probe must agree with a full remux on tracks, chapters and duration.
	outMKV := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, outMKV); err != nil {
		t.Fatalf("RemuxFromMP4: %v", err)
	}
	full, _ := readMKV(t, outMKV)
	if len(full.Tracks) != len(c.Tracks) {
		t.Errorf("track count: probe=%d remux=%d", len(c.Tracks), len(full.Tracks))
	}
	if full.DurationMs != c.DurationMs {
		t.Errorf("duration: probe=%d remux=%d", c.DurationMs, full.DurationMs)
	}
	if len(full.Chapters) != len(c.Chapters) {
		t.Errorf("chapter count: probe=%d remux=%d", len(c.Chapters), len(full.Chapters))
	}
}

// TestReadMetaFromReader exercises the FS-free, seekable ReadMeta entry point.
func TestReadMetaFromReader(t *testing.T) {
	mp4Path := buildTestMP4(t)
	f, err := os.Open(mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	c, _, err := ReadMeta(context.Background(), f, "label.mp4")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if c.Path != "label.mp4" {
		t.Errorf("Path = %q, want label.mp4", c.Path)
	}
	if len(c.Tracks) != 2 {
		t.Errorf("got %d tracks, want 2", len(c.Tracks))
	}
}

// TestReadMetaCancelled checks a cancelled context short-circuits the probe.
func TestReadMetaCancelled(t *testing.T) {
	mp4Path := buildTestMP4(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := OpenMeta(ctx, mp4Path); err == nil {
		t.Fatal("OpenMeta with a cancelled context should error")
	}
}

// TestOpenMetaNoMoov checks a file without a moov box is rejected, like the
// full remux path.
func TestOpenMetaNoMoov(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.mp4")
	// ftyp only, no moov.
	if err := os.WriteFile(bad, []byte{0, 0, 0, 0x10, 'f', 't', 'y', 'p', 'm', 'p', '4', '2', 0, 0, 0, 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenMeta(context.Background(), bad); err == nil {
		t.Fatal("OpenMeta on a moov-less file should error")
	}
}
