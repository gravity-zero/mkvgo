package commands

import "testing"

// TestShiftLinesTrackOrder: the applied shifts print in track order on every
// call, whatever order the map yields them in.
func TestShiftLinesTrackOrder(t *testing.T) {
	shift := map[uint64]int64{5: 40_000_000, 2: -896_000_000, 4: 0, 3: -896_000_000}
	want := "  track 2 shifted by -896 ms\n  track 3 shifted by -896 ms\n  track 4 shifted by +0 ms\n  track 5 shifted by +40 ms\n"
	for run := 0; run < 20; run++ {
		if got := shiftLines(shift); got != want {
			t.Fatalf("run %d: got\n%swant\n%s", run, got, want)
		}
	}
}
