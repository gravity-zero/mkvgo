package matroska

import (
	"context"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

const sampleASS = `[Script Info]
Title: Test
ScriptType: v4.00+
PlayResX: 1920
PlayResY: 1080

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,48,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,2,2,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:01.00,0:00:03.50,Default,,0,0,0,,Hello World
Dialogue: 0,0:00:05.00,0:00:08.00,Default,,0,0,0,,Second line with {\i1}italic{\i0}
Dialogue: 0,0:01:00.00,0:01:05.00,Default,,0,0,0,,Line with, commas, in text
`

func TestParseASSTimestamp(t *testing.T) {
	for _, tt := range []struct {
		s    string
		want int64
	}{
		{"0:00:00.00", 0},
		{"0:00:01.00", 1000},
		{"0:00:01.50", 1500},
		{"1:23:45.67", 5025670},
	} {
		got, ok := subtitle.ParseASSTimestamp(tt.s)
		if !ok {
			t.Errorf("subtitle.ParseASSTimestamp(%q) failed", tt.s)
			continue
		}
		assertEqual(t, got, tt.want, tt.s)
	}
}

func TestFormatASSTimestamp(t *testing.T) {
	for _, tt := range []struct {
		ms   int64
		want string
	}{
		{0, "0:00:00.00"},
		{1500, "0:00:01.50"},
		{5025670, "1:23:45.67"},
	} {
		assertEqual(t, FormatASSTimestamp(tt.ms), tt.want, "FormatASSTimestamp")
	}
}

func TestParseASS(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.ass"
	assertNoErr(t, os.WriteFile(path, []byte(sampleASS), 0644))

	ass, err := ParseASS(path)
	assertNoErr(t, err)
	assertEqual(t, len(ass.Events), 3, "events")
	assertEqual(t, ass.Events[0].StartMs, int64(1000), "event[0].start")
	assertEqual(t, ass.Events[0].EndMs, int64(3500), "event[0].end")
	if !contains(ass.Header, "[Script Info]") {
		t.Error("header should contain [Script Info]")
	}
	if !contains(ass.Header, "Format:") {
		t.Error("header should contain Format line")
	}
}

func TestParseASSEmpty(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/empty.ass"
	assertNoErr(t, os.WriteFile(path, []byte("[Script Info]\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n"), 0644))

	ass, err := ParseASS(path)
	assertNoErr(t, err)
	assertEqual(t, len(ass.Events), 0, "events")
}

func TestMergeASS(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	assPath := dir + "/test.ass"
	assertNoErr(t, os.WriteFile(assPath, []byte(sampleASS), 0644))

	outPath := dir + "/with_ass.mkv"
	assertNoErr(t, MergeASS(context.Background(), fixturePath, assPath, outPath, "eng", "English ASS"))

	c, err := Open(context.Background(), outPath)
	assertNoErr(t, err)
	assertEqual(t, len(c.Tracks), 3, "tracks")
	assertEqual(t, c.Tracks[2].Type, SubtitleTrack, "track type")
	assertEqual(t, c.Tracks[2].Codec, "ass", "codec")
	assertEqual(t, c.Tracks[2].Language, "eng", "lang")
	if len(c.Tracks[2].CodecPrivate) == 0 {
		t.Error("CodecPrivate should contain ASS header")
	}

	counts := countBlocks(t, outPath, c.Info.TimecodeScale)
	t.Logf("blocks: %v", counts)
	assertEqual(t, counts[3], 3, "ASS blocks")
}

func TestExtractASSFacade(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	assPath := dir + "/test.ass"
	assertNoErr(t, os.WriteFile(assPath, []byte(sampleASS), 0644))

	mkvPath := dir + "/with_ass.mkv"
	assertNoErr(t, MergeASS(context.Background(), fixturePath, assPath, mkvPath, "eng", "English ASS"))

	c, err := Open(context.Background(), mkvPath)
	assertNoErr(t, err)

	var assTrackID uint64
	for _, tr := range c.Tracks {
		if tr.Codec == "ass" {
			assTrackID = tr.ID
			break
		}
	}
	if assTrackID == 0 {
		t.Fatal("no ASS track found")
	}

	outPath := dir + "/extracted.ass"
	assertNoErr(t, ExtractASS(context.Background(), mkvPath, assTrackID, outPath))

	data, err := os.ReadFile(outPath)
	assertNoErr(t, err)
	if !contains(string(data), "Dialogue:") {
		t.Fatal("expected Dialogue lines in extracted ASS")
	}
}

func TestMergeASSEmpty(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	assPath := dir + "/empty.ass"
	assertNoErr(t, os.WriteFile(assPath, []byte("[Script Info]\n[Events]\nFormat: x\n"), 0644))

	err := MergeASS(context.Background(), fixturePath, assPath, dir+"/out.mkv", "eng", "")
	if err == nil {
		t.Fatal("expected error for empty ASS")
	}
}
