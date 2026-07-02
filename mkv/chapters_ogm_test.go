package mkv

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseOGMChapters(t *testing.T) {
	in := `# a comment
CHAPTER01=00:00:00.000
CHAPTER01NAME=Intro
CHAPTER02=00:05:12.500
CHAPTER02NAME=Part One
CHAPTER03=01:00:00.000
CHAPTER03NAME=
`
	chapters, err := ParseOGMChapters(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []Chapter{
		{ID: 1, Title: "Intro", StartMs: 0, EndMs: 312500},
		{ID: 2, Title: "Part One", StartMs: 312500, EndMs: 3600000},
		{ID: 3, Title: "", StartMs: 3600000, EndMs: 0},
	}
	if len(chapters) != len(want) {
		t.Fatalf("chapters = %+v, want %d entries", chapters, len(want))
	}
	for i, w := range want {
		g := chapters[i]
		if g.Title != w.Title || g.StartMs != w.StartMs || g.EndMs != w.EndMs {
			t.Errorf("chapter[%d] = {%q %d-%d}, want {%q %d-%d}",
				i, g.Title, g.StartMs, g.EndMs, w.Title, w.StartMs, w.EndMs)
		}
	}

	for _, bad := range []string{
		"", "CHAPTER01 00:00:00.000", "CHAPTER01=99:99:99", "CHAPTERXX=00:00:00.000",
		"NOTACHAPTER=00:00:00.000", "CHAPTER01=00:61:00.000",
	} {
		if _, err := ParseOGMChapters(strings.NewReader(bad)); err == nil {
			t.Errorf("ParseOGMChapters(%q): expected error", bad)
		}
	}
}

func TestFormatOGMChaptersRoundTrip(t *testing.T) {
	src := []Chapter{
		{ID: 1, Title: "B", StartMs: 90500, EndMs: 3661001},
		{ID: 2, Title: "A", StartMs: 0, EndMs: 90500}, // out of order on purpose
		{ID: 3, Title: "C", StartMs: 3661001},
	}
	var buf bytes.Buffer
	if err := FormatOGMChapters(&buf, src); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "CHAPTER01=00:00:00.000\nCHAPTER01NAME=A") ||
		!strings.Contains(out, "CHAPTER02=00:01:30.500\nCHAPTER02NAME=B") ||
		!strings.Contains(out, "CHAPTER03=01:01:01.001\nCHAPTER03NAME=C") {
		t.Fatalf("unexpected output:\n%s", out)
	}

	back, err := ParseOGMChapters(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 3 || back[0].Title != "A" || back[1].StartMs != 90500 || back[2].StartMs != 3661001 {
		t.Errorf("round trip = %+v", back)
	}
}
