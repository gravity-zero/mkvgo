package commands_test

import (
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

func TestParseTimePoint(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"300000", 300000},    // plain milliseconds
		{"90.5", 90500},       // fractional seconds
		{"5:00", 300000},      // MM:SS
		{"1:30", 90000},       // MM:SS
		{"1:30.5", 90500},     // MM:SS.fraction
		{"01:30:00", 5400000}, // HH:MM:SS
		{" 1:00 ", 60000},     // trimmed
	} {
		got, err := commands.ParseTimePoint(tc.in)
		if err != nil {
			t.Errorf("ParseTimePoint(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseTimePoint(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"1:2:3:4", "x:00", "-1:00", "1:-5", ""} {
		if _, err := commands.ParseTimePoint(bad); err == nil {
			t.Errorf("ParseTimePoint(%q): expected error", bad)
		}
	}
}
