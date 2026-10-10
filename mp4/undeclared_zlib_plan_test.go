package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// An SRT track whose blocks are zlib streams without a declaration is served
// inflated by the plan and the full pass alike, and counted; a raw cue that
// begins with "x^" (a valid zlib header pair) is served verbatim.
func TestPlanServesUndeclaredZlibSubtitles(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var blocks []genBlock
	for i := 0; i < 100; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: iframeTestFrame(i, i%25 == 0)})
	}
	for i := 0; i < 3; i++ {
		blocks = append(blocks, genBlock{track: 3, pts: int64(i) * 1000, key: true, data: zlibBytes([]byte("compressed cue " + string(rune('A'+i))))})
		blocks = append(blocks, genBlock{track: 4, pts: int64(i) * 1000, key: true, data: []byte("x^2 + y^2 = z^2 cue " + string(rune('A'+i)))})
	}
	sortGenBlocks(blocks)
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"},
		{ID: 4, Type: mkv.SubtitleTrack, Codec: "srt", Language: "eng"},
	}, blocks)
	ctx := context.Background()
	dir := t.TempDir()
	if err := RemuxToHLS(ctx, src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"sub1.vtt": "compressed cue A", "sub2.vtt": "x^2 + y^2 = z^2 cue A"} {
		got, _, err := plan.Resource(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		full, _ := os.ReadFile(filepath.Join(dir, name))
		if !bytes.Equal(got, full) {
			t.Errorf("%s: plan differs from the full pass", name)
		}
		if !strings.Contains(string(got), want) {
			t.Errorf("%s lacks %q:\n%s", name, want, got)
		}
	}
	if st := plan.Stats(); st.UndeclaredZlibBlocks != 3 {
		t.Errorf("UndeclaredZlibBlocks = %d, want the 3 compressed cues", st.UndeclaredZlibBlocks)
	}
}
