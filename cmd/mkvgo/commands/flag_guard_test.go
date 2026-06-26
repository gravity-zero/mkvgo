package commands

import "testing"

// TestRejectFlagArg verifies that rejectFlagArg triggers Fatal for any arg that
// starts with '-' and has length > 1, and is a no-op for plain filenames and
// the lone-dash stdin sentinel.
func TestRejectFlagArg(t *testing.T) {
	// These must NOT trigger Fatal.
	safe := []string{"file.mkv", "-", "output.mp4", "some/path/to/file", ""}
	for _, a := range safe {
		rejectFlagArg(a) // must return normally
	}

	// These MUST trigger Fatal.
	triggers := []string{"--bogus", "-x", "--faststart", "-fastart", "--", "--skip-unsupported"}
	for _, a := range triggers {
		a := a
		t.Run(a, func(t *testing.T) {
			called := false
			old := osExit
			osExit = func(int) { called = true; panic("exit") }
			defer func() {
				osExit = old
				_ = recover()
				if !called {
					t.Errorf("rejectFlagArg(%q): expected Fatal, got none", a)
				}
			}()
			rejectFlagArg(a)
			t.Errorf("rejectFlagArg(%q): returned without calling Fatal", a)
		})
	}
}
