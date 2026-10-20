package subtitle

import (
	"strings"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/internal/costguard"
)

// A cue made of '<' alone is read in one pass, not once per '<'; cues sharing one start, or starting ever earlier, are
// ended in one pass, as the next later cue would end them.
func TestCueEndCostsGrowLinearly(t *testing.T) {
	costguard.Linear(t, 1<<22, func(n int) time.Duration {
		text := strings.Repeat("<", n)
		start := time.Now()
		ReadingMs(text)
		return costguard.Since(start)
	})
	for name, startAt := range map[string]func(i, n int) int64{
		"one start":    func(int, int) int64 { return 1000 },
		"ever earlier": func(i, n int) int64 { return int64(n - i) },
	} {
		t.Run(name, func(t *testing.T) {
			costguard.Linear(t, 500_000, func(n int) time.Duration {
				cues := make([]Cue, n+1)
				for i := 0; i < n; i++ {
					cues[i] = Cue{StartMs: startAt(i, n), Text: "a"}
				}
				cues[n] = Cue{StartMs: int64(4 * n), EndMs: int64(4*n + 1), Text: "a"}
				start := time.Now()
				SettleCueEnds(cues, 0)
				d := costguard.Since(start)
				for i := 0; i < n; i++ {
					if cues[i].EndMs != cues[i].StartMs+minReadingMs { // read before the one cue starting later
						t.Fatalf("cue %d ends at %d, want %d", i+1, cues[i].EndMs, cues[i].StartMs+minReadingMs)
					}
				}
				return d
			})
		})
	}
}
