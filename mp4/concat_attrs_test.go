package mp4

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestConcatMasterAttributes covers the concat master EXT-X-STREAM-INF assembly
// (concat.go): RESOLUTION/FRAME-RATE/CODECS/AUDIO are emitted only when the
// underlying metadata is present. Mutation testing showed these conditionals
// survived - no test distinguished present from absent.
func TestConcatMasterAttributes(t *testing.T) {
	ctx := context.Background()
	fr := 30.0
	ch := uint8(2)

	t.Run("full-metadata", func(t *testing.T) {
		mk := func() string {
			v := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(1920), Height: u32(1080), FrameRate: &fr}
			a := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: &ch, Language: "eng"}
			return buildABRVariant(t, v, a)
		}
		dir := t.TempDir()
		if err := RemuxConcatToHLS(ctx, []string{mk(), mk()}, dir, Options{SegmentMs: 1000}); err != nil {
			t.Fatal(err)
		}
		master := readTextFile(t, filepath.Join(dir, "master.m3u8"))
		for _, want := range []string{"RESOLUTION=1920x1080", "FRAME-RATE=30.000", "CODECS=", `AUDIO="aud"`} {
			mustContain(t, master, want)
		}
	})

	t.Run("no-frame-rate", func(t *testing.T) {
		mk := func() string {
			v := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(640), Height: u32(360)}
			a := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: &ch}
			return buildABRVariant(t, v, a)
		}
		dir := t.TempDir()
		if err := RemuxConcatToHLS(ctx, []string{mk(), mk()}, dir, Options{SegmentMs: 1000}); err != nil {
			t.Fatal(err)
		}
		master := readTextFile(t, filepath.Join(dir, "master.m3u8"))
		mustContain(t, master, "RESOLUTION=640x360") // dimensions present
		if strings.Contains(master, "FRAME-RATE=") {
			t.Errorf("master should omit FRAME-RATE when the source has none:\n%s", master)
		}
	})
}
