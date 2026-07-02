package mkv

import (
	"strings"
	"testing"
)

// FuzzParseOGMChapters feeds arbitrary text to the OGM chapter parser
// (user-supplied files via `mkvgo set-chapters`). It must never panic, and a
// successful parse must yield ordered, non-negative chapters.
func FuzzParseOGMChapters(f *testing.F) {
	f.Add("CHAPTER01=00:00:00.000\nCHAPTER01NAME=Intro\n")
	f.Add("CHAPTER01=99:59:59.999\nCHAPTER02=00:00:01\n")
	f.Add("# comment\n\nCHAPTER10NAME=only a name\n")
	f.Add("CHAPTER01=00:00:00.000000000001\n")
	f.Add(strings.Repeat("CHAPTER01=00:00:00.000\n", 100))
	f.Add("CHAPTER99999999999999999999=00:00:00.000\n")

	f.Fuzz(func(t *testing.T, data string) {
		chapters, err := ParseOGMChapters(strings.NewReader(data))
		if err != nil {
			return
		}
		if len(chapters) == 0 {
			t.Fatal("nil error with no chapters")
		}
		prev := int64(-1)
		for i, ch := range chapters {
			if ch.StartMs < 0 {
				t.Fatalf("chapter[%d] negative StartMs %d", i, ch.StartMs)
			}
			if ch.StartMs < prev {
				t.Fatalf("chapters not ordered: %d after %d", ch.StartMs, prev)
			}
			if ch.EndMs != 0 && ch.EndMs < ch.StartMs {
				t.Fatalf("chapter[%d] EndMs %d < StartMs %d", i, ch.EndMs, ch.StartMs)
			}
			prev = ch.StartMs
		}
	})
}
