package subtitle

import (
	"strings"
	"testing"
	"time"
)

// costBudget is what a forged input may cost a linear pass; the quadratic ones these tests replace took minutes.
const costBudget = 2 * time.Second

// A cue made of '<' alone is read in one pass, not once per '<'.
func TestReadingMsIsLinear(t *testing.T) {
	text := strings.Repeat("<", 1<<20)
	start := time.Now()
	ReadingMs(text)
	if d := time.Since(start); d > costBudget {
		t.Errorf("ReadingMs of 1 MiB of '<' took %v", d)
	}
}

// Cues sharing one start, or starting ever earlier, are ended in one pass, as the next later cue would end them.
func TestSettleCueEndsIsLinear(t *testing.T) {
	const n = 300_000
	for name, startAt := range map[string]func(i int) int64{
		"one start":    func(int) int64 { return 1000 },
		"ever earlier": func(i int) int64 { return int64(n - i) },
	} {
		cues := make([]Cue, n+1)
		for i := 0; i < n; i++ {
			cues[i] = Cue{StartMs: startAt(i), Text: "a"}
		}
		cues[n] = Cue{StartMs: 2_000_000, EndMs: 2_000_001, Text: "a"}
		start := time.Now()
		SettleCueEnds(cues, 0)
		if d := time.Since(start); d > costBudget {
			t.Errorf("%s: SettleCueEnds of %d cues took %v", name, n, d)
		}
		for i := 0; i < n; i++ {
			if cues[i].EndMs != cues[i].StartMs+minReadingMs { // read before the one cue starting later
				t.Fatalf("%s: cue %d ends at %d, want %d", name, i+1, cues[i].EndMs, cues[i].StartMs+minReadingMs)
			}
		}
	}
}
