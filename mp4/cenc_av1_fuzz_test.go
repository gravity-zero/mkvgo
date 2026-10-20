package mp4

import (
	"testing"

	"github.com/gravity-zero/mkvgo/internal/costguard"
)

// FuzzAV1Subsamples: an AV1 sample of any bytes is split into subsamples that cover it exactly, each protected run
// a whole number of AES blocks, or refused; never a panic.
func FuzzAV1Subsamples(f *testing.F) {
	f.Add(av1TemporalUnit(av1TileGroup(40)))
	f.Add(append(av1SequenceHeader(), av1FrameOBUWithHeader(make([]byte, 33))...))
	f.Fuzz(func(t *testing.T, sample []byte) {
		costguard.Fuzz(t, len(sample), func() {
			subs, err := splitAV1Subsamples(sample)
			if err != nil {
				return
			}
			total := 0
			for _, s := range subs {
				if s.protected%16 != 0 {
					t.Fatalf("protected run of %d bytes is not whole AES blocks", s.protected)
				}
				total += int(s.clear) + int(s.protected)
			}
			if total != len(sample) {
				t.Fatalf("subsamples cover %d bytes of a %d-byte sample", total, len(sample))
			}
		})
	})
}
