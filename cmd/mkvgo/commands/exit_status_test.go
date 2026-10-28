package commands

import (
	"os"
	"path/filepath"
	"testing"
)

// exitStatus runs fn and returns the status it exits with, -1 when it returns.
func exitStatus(t *testing.T, fn func()) (code int) {
	t.Helper()
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	oldExit, oldErr := osExit, os.Stderr
	osExit, os.Stderr = func(c int) { code = c; panic(errExited) }, stderr
	defer func() {
		osExit, os.Stderr = oldExit, oldErr
		stderr.Close()
		if r := recover(); r != nil && r != errExited {
			panic(r)
		}
	}()
	code = -1
	fn()
	return code
}

// TestExitStatusUsage: a command used wrongly exits 2, a command that fails on
// what it reads exits 1.
func TestExitStatusUsage(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.mkv")
	out := filepath.Join(dir, "out.mkv")

	usage := map[string]func(){
		"missing argument":          func() { CmdReindex(nil) },
		"unknown flag":              func() { CmdReindex([]string{"--bogus", missing, out}) },
		"flag without its value":    func() { CmdReindex([]string{missing, out, "--rollback-delta"}) },
		"flags that do not go":      func() { CmdReindex([]string{"--clean-cut", missing, out}) },
		"invalid value":             func() { parseSegmentSeconds("abc") },
		"invalid range":             func() { CmdSplit([]string{missing, "-o", dir, "-range", "nope"}) },
		"required flag missing":     func() { CmdRetime([]string{missing}) },
		"positional that is a flag": func() { rejectFlagArg("-x") },
	}
	for name, fn := range usage {
		if got := exitStatus(t, fn); got != 2 {
			t.Errorf("%s: exit status %d, want 2", name, got)
		}
	}
	if got := exitStatus(t, func() { CmdReindex([]string{missing, out}) }); got != 1 {
		t.Errorf("a source that cannot be read: exit status %d, want 1", got)
	}
}
