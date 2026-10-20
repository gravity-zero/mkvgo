package subtitle

import (
	"strings"
	"testing"
)

func TestReadingMs(t *testing.T) {
	for _, c := range []struct {
		text string
		want int64
	}{
		{"Oui.", 1000},                                  // short: held a second
		{strings.Repeat("a", 34), 2000},                 // 17 a second
		{strings.Repeat("a", 500), 7000},                // long: seven seconds at most
		{`{\an8}{\i1}` + strings.Repeat("a", 34), 2000}, // ASS overrides are not read
		{"<i>" + strings.Repeat("a", 34) + "</i>", 2000},
		{strings.Repeat("a", 17) + `\N` + strings.Repeat("a", 16), 2000}, // an ASS line break reads as a space
		{strings.Repeat("é", 34), 2000},                                  // characters, not bytes
		{"I <3 you" + strings.Repeat("a", 26), 2000},                     // a '<' no '>' closes is text
	} {
		if got := ReadingMs(c.text); got != c.want {
			t.Errorf("ReadingMs(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}

func TestCueEndMs(t *testing.T) {
	long := strings.Repeat("a", 85) // 5 s to read
	for _, c := range []struct {
		name              string
		next              int64
		hasNext           bool
		mediaEnd, wantEnd int64
	}{
		{"read before the next cue", 20_000, true, 0, 15_000},
		{"cut by the next cue", 11_000, true, 0, 11_000},
		{"a next cue at the same time does not end it", 10_000, true, 0, 15_000},
		{"the last cue, read", 0, false, 0, 15_000},
		{"the last cue, cut by the media's end", 0, false, 12_000, 12_000},
		{"a media end before the start is unknown", 0, false, 9_000, 15_000},
	} {
		if got := CueEndMs(10_000, long, c.next, c.hasNext, c.mediaEnd); got != c.wantEnd {
			t.Errorf("%s: end %d, want %d", c.name, got, c.wantEnd)
		}
	}
}

func TestSettleCueEnds(t *testing.T) {
	cues := []Cue{
		{StartMs: 0, EndMs: 500, Text: "stated"},
		{StartMs: 1000, Text: "Oui."},
		{StartMs: 1000, Text: "simultané"},
		{StartMs: 1500, Text: strings.Repeat("a", 85)},
		{StartMs: 60_000, Text: strings.Repeat("a", 85)},
	}
	SettleCueEnds(cues, 62_000)
	for i, want := range []int64{500, 1500, 1500, 6500, 62_000} {
		if cues[i].EndMs != want {
			t.Errorf("cue %d ends at %d, want %d", i+1, cues[i].EndMs, want)
		}
	}
}
