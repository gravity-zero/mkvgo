package commands

import (
	"strings"
	"testing"
)

// TestSegmentSecondsBounded: a -segment the milliseconds cannot hold (1e300, Inf, NaN) is refused by name, as 0 and
// negatives are, instead of wrapping to a negative length.
func TestSegmentSecondsBounded(t *testing.T) {
	c := captureFatals(t)
	for _, v := range []string{"1e300", "Inf", "NaN", "0", "-2", "86401", "0.0004"} {
		if fatal := c.run(func() { parseSegmentSeconds(v) }); !strings.Contains(fatal, "invalid -segment duration") {
			t.Errorf("-segment %s accepted", v)
		}
	}
	for v, want := range map[string]int64{"6": 6000, "0.5": 500, "0.001": 1, "86400": 86_400_000} {
		if got := parseSegmentSeconds(v); got != want {
			t.Errorf("-segment %s = %d ms, want %d", v, got, want)
		}
	}
	if fatal := c.run(func() { parseServeGrowingFlags([]string{"-segment", "1e300", "in.mkv"}) }); fatal == "" {
		t.Error("serve-growing -segment 1e300 accepted")
	}
}
