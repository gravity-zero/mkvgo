package commands

import "testing"

// helpers_internal_test.go - white-box tests for unexported parsing helpers.

// TestSplitTrackSpec locks in that the file:trackID parser splits on the LAST
// colon, so a path that itself contains a colon (a Windows drive letter, or a
// colon-bearing filename on Unix) is preserved. Both CmdMux and CmdAddTrack
// route through this helper, so this guards the whole class of bug at once.
func TestSplitTrackSpec(t *testing.T) {
	cases := []struct {
		in       string
		path, id string
		ok       bool
	}{
		{`sample.mkv:1`, `sample.mkv`, `1`, true},
		{`C:\Users\me\sample.mkv:2`, `C:\Users\me\sample.mkv`, `2`, true},
		{`/tmp/a:b/sample.mkv:3`, `/tmp/a:b/sample.mkv`, `3`, true},
		{`no-colon.mkv`, ``, ``, false},
		{`:5`, ``, ``, false},          // empty path
		{`sample.mkv:`, ``, ``, false}, // empty trackID
		{``, ``, ``, false},
	}
	for _, c := range cases {
		path, id, ok := splitTrackSpec(c.in)
		if ok != c.ok || path != c.path || id != c.id {
			t.Errorf("splitTrackSpec(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, path, id, ok, c.path, c.id, c.ok)
		}
	}
}
