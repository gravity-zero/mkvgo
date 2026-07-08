package commands

import "testing"

// TestParseHLSFlagsSubOffset checks --sub-offset parses a plain integer
// (positive, negative, or zero) into hlsFlags.subOffset, and that it rides
// through to Options.SubtitleOffsetMs.
func TestParseHLSFlagsSubOffset(t *testing.T) {
	cases := []struct {
		args []string
		want int64
	}{
		{[]string{"in.mkv", "--sub-offset", "500"}, 500},
		{[]string{"in.mkv", "--sub-offset", "-500"}, -500},
		{[]string{"in.mkv", "--sub-offset", "0"}, 0},
	}
	for _, c := range cases {
		f := parseHLSFlags(c.args)
		if f.subOffset != c.want {
			t.Errorf("parseHLSFlags(%v).subOffset = %d, want %d", c.args, f.subOffset, c.want)
		}
		if got := f.options("in.mkv").SubtitleOffsetMs; got != c.want {
			t.Errorf("options(%v).SubtitleOffsetMs = %d, want %d", c.args, got, c.want)
		}
	}
}

// A non-integer --sub-offset value is a usage error (Fatal), not a silent 0.
func TestParseHLSFlagsSubOffsetInvalid(t *testing.T) {
	called := false
	old := osExit
	osExit = func(int) { called = true; panic("exit") }
	defer func() {
		osExit = old
		_ = recover()
		if !called {
			t.Error("--sub-offset with a non-integer value: expected Fatal, got none")
		}
	}()
	parseHLSFlags([]string{"in.mkv", "--sub-offset", "notanumber"})
	t.Error("parseHLSFlags returned without calling Fatal")
}
