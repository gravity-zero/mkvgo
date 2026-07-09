package mp4

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// buildABRVariant writes an MKV with the given video track and audio tracks and
// enough keyframed blocks to segment, for exercising the ABR master and DASH
// attribute logic with controlled track metadata.
func buildABRVariant(t *testing.T, video mkv.Track, audios ...mkv.Track) string {
	t.Helper()
	var blocks []genBlock
	for i := 0; i < 60; i++ {
		blocks = append(blocks, genBlock{track: video.ID, pts: int64(i) * 40, key: i%25 == 0, data: cencVideoSample()})
	}
	for _, a := range audios {
		for i := 0; i < 120; i++ {
			blocks = append(blocks, genBlock{track: a.ID, pts: int64(i) * 20, key: true, data: cencAudioSample(i)})
		}
	}
	sortGenBlocks(blocks)
	tracks := append([]mkv.Track{video}, audios...)
	return buildMKV(t, tracks, blocks)
}

func u32(v uint32) *uint32 { return &v }

// TestABRMasterAndDASHAttributes_Present exercises the branches that emit each
// optional attribute (audio name/language/DEFAULT, DASH width/height/frameRate/
// lang/audioSamplingRate) with metadata that sets all of them - and their
// absent counterparts. Mutation testing (gremlins) showed these conditionals in
// abr.go survived: no test distinguished the "attribute present" from the
// "attribute absent" branch.
func TestABRMasterAndDASHAttributes(t *testing.T) {
	ctx := context.Background()
	sr := 48000.0
	fr := 30.0
	ch := uint8(2)

	t.Run("rich-metadata-and-explicit-default", func(t *testing.T) {
		video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(1920), Height: u32(1080), FrameRate: &fr}
		audio := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Name: "Commentary", Language: "fra", IsDefault: true}
		src := buildABRVariant(t, video, audio)
		dir := t.TempDir()
		if err := RemuxToABR(ctx, []string{src, src}, dir, Options{SegmentMs: 1000}); err != nil {
			t.Fatal(err)
		}
		master := readTextFile(t, filepath.Join(dir, "master.m3u8"))
		mustContain(t, master, `NAME="Commentary"`)
		mustContain(t, master, `LANGUAGE="fra"`)
		mustContain(t, master, `DEFAULT=YES`)
		mpd := readTextFile(t, filepath.Join(dir, "manifest.mpd"))
		mustContain(t, mpd, `width="1920"`)
		mustContain(t, mpd, `height="1080"`)
		mustContain(t, mpd, `frameRate=`)
		mustContain(t, mpd, `lang="fra"`)
		mustContain(t, mpd, `audioSamplingRate="48000"`)
	})

	t.Run("minimal-metadata", func(t *testing.T) {
		// No name, no language, no frame rate, no sample rate: the master must
		// fall back to "Audio 1" for the name and omit LANGUAGE; the DASH must
		// omit frameRate, lang and audioSamplingRate.
		video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(640), Height: u32(360)}
		audio := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: &ch}
		src := buildABRVariant(t, video, audio)
		dir := t.TempDir()
		if err := RemuxToABR(ctx, []string{src, src}, dir, Options{SegmentMs: 1000}); err != nil {
			t.Fatal(err)
		}
		master := readTextFile(t, filepath.Join(dir, "master.m3u8"))
		mustContain(t, master, `NAME="Audio 1"`)
		mustNotContain(t, master, `LANGUAGE=`)
		mustContain(t, master, `DEFAULT=YES`) // only audio, no explicit default -> first is default
		mpd := readTextFile(t, filepath.Join(dir, "manifest.mpd"))
		mustNotContain(t, mpd, `frameRate=`)
		mustNotContain(t, mpd, ` lang=`)
		mustNotContain(t, mpd, `audioSamplingRate=`)
	})

	t.Run("two-audio-none-default", func(t *testing.T) {
		// With two audio tracks and none flagged default, only the first gets
		// DEFAULT=YES (nAudio == 1); the second must not.
		video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(640), Height: u32(360)}
		a1 := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: &ch, Language: "eng"}
		a2 := mkv.Track{ID: 3, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: &ch, Language: "fra"}
		src := buildABRVariant(t, video, a1, a2)
		dir := t.TempDir()
		if err := RemuxToABR(ctx, []string{src, src}, dir, Options{SegmentMs: 1000}); err != nil {
			t.Fatal(err)
		}
		master := readTextFile(t, filepath.Join(dir, "master.m3u8"))
		lines := strings.Split(master, "\n")
		defaults := 0
		for _, l := range lines {
			if strings.HasPrefix(l, "#EXT-X-MEDIA:TYPE=AUDIO") && strings.Contains(l, "DEFAULT=YES") {
				defaults++
			}
		}
		if defaults != 1 {
			t.Errorf("want exactly one audio rendition with DEFAULT=YES, got %d:\n%s", defaults, master)
		}
	})
}

func readTextFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("expected to contain %q:\n%s", sub, s)
	}
}

func mustNotContain(t *testing.T, s, sub string) {
	t.Helper()
	if strings.Contains(s, sub) {
		t.Errorf("expected NOT to contain %q:\n%s", sub, s)
	}
}
