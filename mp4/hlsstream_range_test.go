package mp4

import (
	"context"
	"math"
	"testing"
)

// A range whose end overflows int64 is refused, for a streamed segment and for bytes in hand.
func TestWriteRangeRefusesOverflowingRange(t *testing.T) {
	src := openGOPSource(t, 3, false, false)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"seg00001.m4s", "init.mp4"} {
		h, err := plan.Open(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range []int64{math.MaxInt64, math.MaxInt64 - 100, h.Size()} {
			var sink tallyWriter
			m, err := h.WriteRange(context.Background(), &sink, 100, n)
			if err == nil || m != 0 || sink.n != 0 {
				t.Errorf("%s: WriteRange(100, %d) = %d, %v with %d bytes written, want a refusal", name, n, m, err, sink.n)
			}
		}
	}
}

type tallyWriter struct{ n int }

func (c *tallyWriter) Write(p []byte) (int, error) { c.n += len(p); return len(p), nil }
