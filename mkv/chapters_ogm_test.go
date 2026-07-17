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

// TestParseOGMChaptersHoursOverflow pins both sides of what the hours field
// can spell. The format puts no ceiling on HH, and minutes to milliseconds
// are all range checked, so the hours are the one place an entry can name a
// time that does not fit int64 milliseconds. Unchecked, the multiply wrapped
// and 2700000000000:0:0 came back as a chapter starting 276 million years
// before the file: a negative StartMs, not an error.
func TestParseOGMChaptersHoursOverflow(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int64 // only when ok
		ok   bool
	}{
		// 2562047788015*3600000 + 12*60000 + 55*1000 + 807 is exactly MaxInt64.
		{"largest representable", "CHAPTER01=2562047788015:12:55.807", 1<<63 - 1, true},
		{"one millisecond past it", "CHAPTER01=2562047788015:12:55.808", 0, false},
		{"one hour past the hours bound", "CHAPTER01=2562047788016:00:00.000", 0, false},
		{"the hours field wraps on its own", "CHAPTER01=9223372036854775807:0:0", 0, false},
		{"what the fuzzer found", "CHAPTER01=2700000000000:0:0", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseOGMChapters(strings.NewReader(tc.in))
			switch {
			case !tc.ok:
				if err == nil {
					t.Fatalf("accepted %q as StartMs %d, want an error", tc.in, got[0].StartMs)
				}
			case err != nil:
				t.Fatalf("refused %q: %v", tc.in, err)
			case got[0].StartMs != tc.want:
				t.Fatalf("StartMs = %d, want %d", got[0].StartMs, tc.want)
			}
		})
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
