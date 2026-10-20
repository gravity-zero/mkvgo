package ops

import (
	"testing"

	"github.com/gravity-zero/mkvgo/internal/costguard"
	"github.com/gravity-zero/mkvgo/mkv"
)

// FuzzPGSAssembler: two PGS blocks of any bytes decode and flatten to cues or an error, never a panic, and every cue
// ends after it starts.
func FuzzPGSAssembler(f *testing.F) {
	f.Add(pgsShow(1, 100, 900, true), pgsClear(), int64(0))
	f.Add(pgsShow(1, 0, 0, true), pgsShow(2, 1916, 1078, false), int64(40))
	f.Fuzz(func(t *testing.T, first, second []byte, durMs int64) {
		costguard.Fuzz(t, len(first)+len(second), func() {
			var cues []PGSCue
			a := newPGSAssembler(func(c PGSCue) error { cues = append(cues, c); return nil }, &mkv.Track{Codec: "S_HDMV/PGS"})
			if a.block(1000, durMs%5000, first) != nil || a.block(2000, 0, second) != nil {
				return
			}
			for _, c := range cues {
				if c.EndMs <= c.StartMs {
					t.Fatalf("cue %d..%d ms ends before it starts", c.StartMs, c.EndMs)
				}
			}
		})
	})
}
