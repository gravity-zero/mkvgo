package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// Two pre-encoded qualities become one multi-variant master: the reference
// variant carries audio+subs, the second only its video, and the top master
// declares both with their real attributes over the shared audio group.
func TestRemuxToABR(t *testing.T) {
	build := func(w, h uint32, size int) string {
		sr, ch := 44100.0, uint8(2)
		var gblocks []genBlock
		for i := 0; i < 100; i++ {
			data := append([]byte{0x00, 0x00, 0x00, 0x01, 0x65}, bytes.Repeat([]byte{byte(i)}, size)...)
			gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: data})
		}
		for i := 0; i < 200; i++ {
			gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true,
				data: []byte{0xAA, byte(i)}})
		}
		sortGenBlocks(gblocks)
		return buildMKV(t,
			[]mkv.Track{
				{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
				{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
			},
			gblocks)
	}
	hi := build(1280, 720, 400)
	lo := build(640, 360, 100)

	dir := t.TempDir()
	if err := RemuxToABR(context.Background(), []string{hi, lo}, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}

	master, err := os.ReadFile(filepath.Join(dir, "master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"RESOLUTION=1280x720", "RESOLUTION=640x360",
		"v1/playlist.m3u8", "v2/playlist.m3u8",
		`URI="v1/audio1.m3u8"`, `AUDIO="aud"`, `CODECS="avc1.64001F,mp4a.40.2"`,
	} {
		if !bytes.Contains(master, []byte(want)) {
			t.Errorf("ABR master missing %q:\n%s", want, master)
		}
	}
	if bytes.Count(master, []byte("#EXT-X-STREAM-INF:")) != 2 {
		t.Errorf("want 2 variants:\n%s", master)
	}

	// The reference variant is complete; the second is video-only.
	if _, err := os.Stat(filepath.Join(dir, "v1/init_a1.mp4")); err != nil {
		t.Error("v1 must carry the audio rendition")
	}
	if _, err := os.Stat(filepath.Join(dir, "v2/init_a1.mp4")); !os.IsNotExist(err) {
		t.Error("v2 must be video-only")
	}
	if _, err := os.Stat(filepath.Join(dir, "v2/seg00001.m4s")); err != nil {
		t.Error("v2 video segments missing")
	}

	// One source is rejected - RemuxToHLS is the tool for that.
	if err := RemuxToABR(context.Background(), []string{hi}, t.TempDir()); err == nil {
		t.Error("single source must be rejected")
	}
}
