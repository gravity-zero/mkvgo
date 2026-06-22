package subtitle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatVTTTime(t *testing.T) {
	cases := []struct {
		ms   int64
		want string
	}{
		{0, "00:00:00.000"},
		{1500, "00:00:01.500"},
		{3661500, "01:01:01.500"},
		{-5, "00:00:00.000"},
	}
	for _, c := range cases {
		if got := FormatVTTTime(c.ms); got != c.want {
			t.Errorf("FormatVTTTime(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}

func TestWriteWebVTT(t *testing.T) {
	var b strings.Builder
	cues := []Cue{
		{StartMs: 0, EndMs: 2000, Text: "Hello"},
		{StartMs: 2000, EndMs: 4000, Text: "World"},
		{StartMs: 4000, EndMs: 5000, Text: ""}, // empty → skipped
	}
	if err := WriteWebVTT(&b, cues); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	want := "WEBVTT\n\n" +
		"00:00:00.000 --> 00:00:02.000\nHello\n\n" +
		"00:00:02.000 --> 00:00:04.000\nWorld\n\n"
	if got != want {
		t.Errorf("WriteWebVTT =\n%q\nwant\n%q", got, want)
	}
}

func TestASSToCues(t *testing.T) {
	events := []ASSEvent{
		{StartMs: 1000, EndMs: 2000, Fields: `Default,,0,0,0,,{\i1}Hello{\i0}\Nworld`},
	}
	cues := ASSToCues(events)
	if len(cues) != 1 || cues[0].Text != "Hello\nworld" {
		t.Errorf("ASSToCues = %+v, want text \"Hello\\nworld\"", cues)
	}
}

func TestFlattenASSBlock(t *testing.T) {
	// S_TEXT/ASS block: ReadOrder,Layer,Style,Name,MarginL,MarginR,MarginV,Effect,Text
	got := FlattenASSBlock([]byte(`0,0,Default,,0,0,0,,{\b1}Bold{\b0}\Ntext`))
	if got != "Bold\ntext" {
		t.Errorf("FlattenASSBlock = %q, want \"Bold\\ntext\"", got)
	}
}

func TestFileToWebVTT(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	srt := write("a.srt", "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n")
	var b strings.Builder
	if err := FileToWebVTT(srt, &b); err != nil {
		t.Fatalf("FileToWebVTT(srt): %v", err)
	}
	if !strings.HasPrefix(b.String(), "WEBVTT") || !strings.Contains(b.String(), "00:00:01.000 --> 00:00:02.000\nHello") {
		t.Errorf("srt→vtt =\n%q", b.String())
	}

	ass := write("a.ass", "[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,{\\i1}Hi{\\i0}\n")
	b.Reset()
	if err := FileToWebVTT(ass, &b); err != nil {
		t.Fatalf("FileToWebVTT(ass): %v", err)
	}
	if !strings.Contains(b.String(), "00:00:01.000 --> 00:00:02.000\nHi") {
		t.Errorf("ass→vtt =\n%q", b.String())
	}

	vtt := write("a.vtt", "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nPassthrough\n")
	b.Reset()
	if err := FileToWebVTT(vtt, &b); err != nil {
		t.Fatalf("FileToWebVTT(vtt): %v", err)
	}
	if b.String() != "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nPassthrough\n" {
		t.Errorf("vtt passthrough =\n%q", b.String())
	}
}

func TestResolveCueEnds(t *testing.T) {
	cues := []Cue{
		{StartMs: 0, Text: "a"},                 // no end → next start 1000
		{StartMs: 1000, EndMs: 1500, Text: "b"}, // explicit end kept
		{StartMs: 2000, Text: "c"},              // no end, last → default
	}
	ResolveCueEnds(cues, 3000)
	if cues[0].EndMs != 1000 || cues[1].EndMs != 1500 || cues[2].EndMs != 5000 {
		t.Errorf("ResolveCueEnds = %+v", cues)
	}
}
