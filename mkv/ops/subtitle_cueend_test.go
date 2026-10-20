package ops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/internal/costguard"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// cueEndFixture is a film of 20 s whose cues state no duration: two a second apart, one before a 10 s silence, and a
// last one 500 ms before the end.
func cueEndFixture(t *testing.T, codec string, cue func(text string) []byte) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	at := map[int64]string{1000: "Oui.", 2000: "Non.", 4000: strings.Repeat("a", 85), 19_500: "Fin."}
	var sets [][]mkv.Block
	for s := int64(0); s < 20; s++ {
		set := []mkv.Block{{TrackNumber: 1, Timecode: s * 1000, Keyframe: true, Data: []byte{0, 0, 0, 1, 0x65, byte(s)}}}
		for _, tc := range []int64{s * 1000, s*1000 + 500} {
			if text, ok := at[tc]; ok {
				set = append(set, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: cue(text)})
			}
		}
		sets = append(sets, set)
	}
	var priv []byte
	if codec == "ass" {
		priv = []byte("[Script Info]\nScriptType: v4.00+\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text")
	}
	return buildMKVCuedBlocks(t, t.TempDir(), "s.mkv", []mkv.Track{
		{ID: 1, UID: 11, Type: mkv.VideoTrack, Codec: "h264", Width: &w, Height: &h},
		{ID: 2, UID: 22, Type: mkv.SubtitleTrack, Codec: codec, CodecPrivate: priv},
	}, sets, 20_000, nil)
}

// A cue that states no end stops at the next cue, once read, or at the film's end, whichever comes first: never over
// the next cue, never through a silence, never past the film, in SRT, ASS and WebVTT alike.
func TestExtractedCuesEndOnceRead(t *testing.T) {
	ctx := context.Background()
	srt := cueEndFixture(t, "srt", func(text string) []byte { return []byte(text) })
	ass := cueEndFixture(t, "ass", func(text string) []byte { return []byte("0,0,Default,,0,0,0,," + text) })
	read := func(path string) string {
		data, err := os.ReadFile(path)
		mustNil(t, err)
		return string(data)
	}
	dir := t.TempDir()
	mustNil(t, ExtractSubtitle(ctx, srt, 2, filepath.Join(dir, "s.srt")))
	mustNil(t, ExtractASS(ctx, ass, 2, filepath.Join(dir, "s.ass")))
	var vtt bytes.Buffer
	mustNil(t, ExtractSubtitleWebVTT(ctx, srt, 2, &vtt))
	for name, c := range map[string]struct {
		got  string
		want []string
	}{
		"srt": {read(filepath.Join(dir, "s.srt")), []string{"00:00:01,000 --> 00:00:02,000", "00:00:02,000 --> 00:00:03,000", "00:00:04,000 --> 00:00:09,000", "00:00:19,500 --> 00:00:20,000"}},
		"ass": {read(filepath.Join(dir, "s.ass")), []string{",0:00:01.00,0:00:02.00,", ",0:00:02.00,0:00:03.00,", ",0:00:04.00,0:00:09.00,", ",0:00:19.50,0:00:20.00,"}},
		"vtt": {vtt.String(), []string{"00:00:01.000 --> 00:00:02.000", "00:00:02.000 --> 00:00:03.000", "00:00:04.000 --> 00:00:09.000", "00:00:19.500 --> 00:00:20.000"}},
	} {
		for _, w := range c.want {
			if !strings.Contains(c.got, w) {
				t.Errorf("%s lacks %q:\n%s", name, w, c.got)
			}
		}
	}
}

// The extraction queue ends each cue at the first later cue that starts after it, as subtitle.SettleCueEnds does, and
// writes the cues in the order met, blocks out of time order included.
func TestOpenCuesFollowSettleCueEnds(t *testing.T) {
	type cue struct{ start, end int64 }
	in := []cue{{1000, 0}, {500, 0}, {800, 0}, {900, 950}, {1200, 0}} // closer than a second: the next cue decides
	q := openCues{mediaEndMs: 2000}
	var got []int64
	for _, c := range in {
		mustNil(t, q.add(c.start, c.end, "Oui.", func(end int64) error { got = append(got, end); return nil }))
	}
	mustNil(t, q.flush(0, false))
	var want []int64
	cues := make([]subtitle.Cue, len(in))
	for i, c := range in {
		cues[i] = subtitle.Cue{StartMs: c.start, EndMs: c.end, Text: "Oui."}
	}
	subtitle.SettleCueEnds(cues, 2000)
	for _, c := range cues {
		want = append(want, c.EndMs)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ends %v, want %v", got, want)
	}
}

// Cues stating no end that arrive ever earlier are queued and ended in one pass, not once per cue met.
func TestOpenCuesCostGrowsLinearly(t *testing.T) {
	costguard.Linear(t, 300_000, func(n int) time.Duration {
		q := openCues{}
		written := 0
		start := time.Now()
		for i := 0; i < n; i++ {
			mustNil(t, q.add(int64(n-i), 0, "a", func(int64) error { written++; return nil }))
		}
		mustNil(t, q.add(int64(4*n), int64(4*n+1), "a", func(int64) error { written++; return nil }))
		mustNil(t, q.flush(0, false))
		d := costguard.Since(start)
		if written != n+1 {
			t.Fatalf("%d cues written, want %d", written, n+1)
		}
		return d
	})
}
