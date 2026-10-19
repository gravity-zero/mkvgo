package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestExtractedCuesEndWhereTheirBlocksSay: SRT and ASS extraction end each cue at its BlockDuration, as WebVTT does,
// where every cue lasted 3 s.
func TestExtractedCuesEndWhereTheirBlocksSay(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	sparseTracksMKV(t, src, []mkv.Track{videoTrack(1)})
	srt, ass := filepath.Join(dir, "in.srt"), filepath.Join(dir, "in.ass")
	mustNil(t, os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:00,500\nShort\n\n2\n00:00:01,000 --> 00:00:05,500\nLong\n"), 0o644))
	mustNil(t, os.WriteFile(ass, []byte("[Script Info]\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:00.00,0:00:00.50,Default,,0,0,0,,Short\nDialogue: 0,0:00:01.00,0:00:05.50,Default,,0,0,0,,Long\n"), 0o644))
	withSRT, withASS := filepath.Join(dir, "srt.mkv"), filepath.Join(dir, "ass.mkv")
	mustNil(t, MergeSubtitle(ctx, src, srt, withSRT, "eng", ""))
	mustNil(t, MergeASS(ctx, src, ass, withASS, "eng", ""))

	out := filepath.Join(dir, "out.srt")
	mustNil(t, ExtractSubtitle(ctx, withSRT, 2, out))
	got, _ := os.ReadFile(out)
	for _, want := range []string{"00:00:00,000 --> 00:00:00,500", "00:00:01,000 --> 00:00:05,500"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("SRT lacks %q:\n%s", want, got)
		}
	}
	outASS := filepath.Join(dir, "out.ass")
	mustNil(t, ExtractASS(ctx, withASS, 2, outASS))
	got, _ = os.ReadFile(outASS)
	for _, want := range []string{"0:00:00.00,0:00:00.50", "0:00:01.00,0:00:05.50"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("ASS lacks %q:\n%s", want, got)
		}
	}
}

// TestExtractSubtitleRefusesABitmapTrackAsSRT: a PGS track written as SRT was binary in a .srt; it is refused with
// the format that extracts it.
func TestExtractSubtitleRefusesABitmapTrackAsSRT(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "pgs.mkv")
	sparseTracksMKV(t, src, []mkv.Track{videoTrack(1), subtitleTrack(2, "pgs")})
	out := filepath.Join(dir, "out.srt")
	if err := ExtractSubtitle(context.Background(), src, 2, out); err == nil || !strings.Contains(err.Error(), "-format pgs") {
		t.Errorf("SRT extraction of a PGS track: %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("an output was written")
	}
}

// TestMergeASSKeepsLayersAndTimeOrder: an ASS listing its events out of time order is muxed in time order, each
// event keeping its layer and its place in the file (ReadOrder).
func TestMergeASSKeepsLayersAndTimeOrder(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	sparseTracksMKV(t, src, []mkv.Track{videoTrack(1)})
	ass := filepath.Join(dir, "in.ass")
	mustNil(t, os.WriteFile(ass, []byte("[Script Info]\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 2,0:00:40.00,0:00:41.00,Default,,0,0,0,,Late\nDialogue: 1,0:00:00.00,0:00:01.00,Default,,0,0,0,,Early\n"), 0o644))
	out := filepath.Join(dir, "out.mkv")
	mustNil(t, MergeASS(ctx, src, ass, out, "eng", ""))
	var got []string
	f, err := os.Open(out)
	mustNil(t, err)
	defer f.Close()
	br, err := reader.NewBlockReader(f, 1_000_000)
	mustNil(t, err)
	for {
		blk, err := br.Next()
		if err != nil {
			break
		}
		if blk.TrackNumber == 2 {
			got = append(got, string(blk.Data))
		}
	}
	if len(got) != 2 || !strings.HasPrefix(got[0], "1,1,Default") || !strings.HasPrefix(got[1], "0,2,Default") {
		t.Errorf("ASS blocks %q, want Early (ReadOrder 1, layer 1) then Late (ReadOrder 0, layer 2)", got)
	}
}
