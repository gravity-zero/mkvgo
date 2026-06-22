package ops

import (
	"context"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func TestExtractSubtitleWebVTT_SRT(t *testing.T) {
	dir := t.TempDir()
	tracks := []mkv.Track{subtitleTrack(1, "srt")}
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 1000, Duration: 1000, Data: []byte("Hello")},
		{TrackNumber: 1, Timecode: 3000, Duration: 2000, Data: []byte("World")},
	}
	path := buildMinimalMKV(t, dir, "sub.mkv", tracks, blocks, 5000)

	var b strings.Builder
	if err := ExtractSubtitleWebVTT(context.Background(), path, 1, &b); err != nil {
		t.Fatalf("ExtractSubtitleWebVTT: %v", err)
	}
	got := b.String()
	for _, want := range []string{
		"WEBVTT",
		"00:00:01.000 --> 00:00:02.000\nHello",
		"00:00:03.000 --> 00:00:05.000\nWorld",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestExtractSubtitleWebVTT_ASS(t *testing.T) {
	dir := t.TempDir()
	tracks := []mkv.Track{subtitleTrack(1, "ass")}
	// S_TEXT/ASS block framing: ReadOrder,Layer,Style,Name,MarginL,MarginR,MarginV,Effect,Text
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 1000, Duration: 1000, Data: []byte(`0,0,Default,,0,0,0,,{\i1}Styled{\i0}`)},
	}
	path := buildMinimalMKV(t, dir, "ass.mkv", tracks, blocks, 3000)

	var b strings.Builder
	if err := ExtractSubtitleWebVTT(context.Background(), path, 1, &b); err != nil {
		t.Fatalf("ExtractSubtitleWebVTT: %v", err)
	}
	if !strings.Contains(b.String(), "00:00:01.000 --> 00:00:02.000\nStyled") {
		t.Errorf("ASS override tags not flattened:\n%s", b.String())
	}
}

func TestExtractSubtitleWebVTT_Errors(t *testing.T) {
	dir := t.TempDir()
	path := buildMinimalMKV(t, dir, "x.mkv",
		[]mkv.Track{subtitleTrack(1, "pgs")}, // bitmap subtitle (not text)
		[]mkv.Block{{TrackNumber: 1, Timecode: 0, Data: []byte{1, 2}}}, 1000)

	var b strings.Builder
	if err := ExtractSubtitleWebVTT(context.Background(), path, 1, &b); err == nil {
		t.Error("expected error for a non-text subtitle codec")
	}
	if err := ExtractSubtitleWebVTT(context.Background(), path, 99, &b); err == nil {
		t.Error("expected error for a missing track")
	}
}
