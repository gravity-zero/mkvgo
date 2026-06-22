package mp4

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestKeyframesMP4 checks OpenMeta fills Container.Keyframes with the video sync
// samples' presentation timestamps — in the same pass, no separate open.
func TestKeyframesMP4(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC,
			Channels: u8p(2), SampleRate: f64p(48000)},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 1, pts: 1000, key: false, data: []byte{2}},
		{track: 1, pts: 2000, key: true, data: []byte{3}},
		{track: 1, pts: 3000, key: false, data: []byte{4}},
		{track: 1, pts: 4000, key: true, data: []byte{5}},
		{track: 2, pts: 0, key: true, data: []byte{6}}, // audio keyframe must be ignored
	}
	srcMKV := buildMKVWithChapters(t, tracks, blocks, nil)
	mp4Path := filepath.Join(t.TempDir(), "in.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}

	c, _, err := OpenMeta(context.Background(), mp4Path)
	if err != nil {
		t.Fatalf("OpenMeta: %v", err)
	}
	want := []int64{0, 2000, 4000}
	if len(c.Keyframes) != len(want) {
		t.Fatalf("Keyframes = %v, want %v", c.Keyframes, want)
	}
	for i := range want {
		if c.Keyframes[i] != want[i] {
			t.Fatalf("Keyframes = %v, want %v", c.Keyframes, want)
		}
	}
}

// TestKeyframesMP4NoVideo checks an audio-only MP4 leaves Keyframes nil.
func TestKeyframesMP4NoVideo(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC,
			Channels: u8p(2), SampleRate: f64p(48000)},
	}
	blocks := []genBlock{{track: 1, pts: 0, key: true, data: []byte{1}}}
	srcMKV := buildMKVWithChapters(t, tracks, blocks, nil)
	mp4Path := filepath.Join(t.TempDir(), "audio.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	c, _, err := OpenMeta(context.Background(), mp4Path)
	if err != nil {
		t.Fatalf("OpenMeta: %v", err)
	}
	if c.Keyframes != nil {
		t.Errorf("Keyframes = %v, want nil for an audio-only file", c.Keyframes)
	}
}

// TestKeyframesMP4FromReader checks ReadMeta also fills Keyframes.
func TestKeyframesMP4FromReader(t *testing.T) {
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
	// buildTestMP4's video track has a single sync sample at pts 0.
	if len(c.Keyframes) != 1 || c.Keyframes[0] != 0 {
		t.Errorf("Keyframes = %v, want [0]", c.Keyframes)
	}
}
