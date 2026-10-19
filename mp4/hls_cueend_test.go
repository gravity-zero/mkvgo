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

// A cue that states no end leaves the screen once read, not at a cue 15 s later, in the full pass and the plan alike.
func TestHLSCueWithoutEndLeavesOnceRead(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	for i := 0; i < 500; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	gblocks = append(gblocks, genBlock{track: 2, pts: 4000, key: true, data: []byte(strings.Repeat("a", 85))},
		genBlock{track: 2, pts: 19_000, key: true, data: []byte("Fin.")})
	sortGenBlocks(gblocks)
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"},
	}, gblocks)
	ctx := context.Background()
	dir := t.TempDir()
	if err := RemuxToHLS(ctx, src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(filepath.Join(dir, "sub1.vtt"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	served, _, err := plan.Resource(ctx, "sub1.vtt")
	if err != nil {
		t.Fatal(err)
	}
	for name, vtt := range map[string][]byte{"full pass": full, "plan": served} {
		if !bytes.Contains(vtt, []byte("00:00:04.000 --> 00:00:09.000")) || !bytes.Contains(vtt, []byte("00:00:19.000 --> 00:00:20.000")) {
			t.Errorf("%s:\n%s", name, vtt)
		}
	}
}

// A remux to MP4 times a cue that states no end by the same rule: once read, at the next cue or at the film's end.
func TestRemuxCueWithoutEndLeavesOnceRead(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	for i := 0; i < 500; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	gblocks = append(gblocks, genBlock{track: 2, pts: 1000, key: true, data: []byte("Oui.")},
		genBlock{track: 2, pts: 4000, key: true, data: []byte(strings.Repeat("a", 85))},
		genBlock{track: 2, pts: 19_500, key: true, data: []byte("Fin.")})
	sortGenBlocks(gblocks)
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"},
	}, gblocks)
	dst := filepath.Join(t.TempDir(), "o.mp4")
	if err := RemuxToMP4(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	var vtt bytes.Buffer
	if err := ExtractSubtitleWebVTT(context.Background(), dst, 2, &vtt); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"00:00:01.000 --> 00:00:02.000", "00:00:04.000 --> 00:00:09.000"} {
		if !strings.Contains(vtt.String(), want) {
			t.Errorf("lacks %q:\n%s", want, vtt.String())
		}
	}
	t.Logf("%s", vtt.String())
}
