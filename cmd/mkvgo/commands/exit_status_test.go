package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// TestExitDamaged: a salvage that found a cut source exits 3 under
// --exit-damaged and 0 without it; a whole source exits 0 either way.
func TestExitDamaged(t *testing.T) {
	whole, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "testdata", "sample.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cut := filepath.Join(dir, "cut.mkv")
	if err := os.WriteFile(cut, whole[:len(whole)*6/10], 0o644); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(dir, "whole.mkv")
	if err := os.WriteFile(full, whole, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ExitDamaged, sawDamage = false, false })

	for i, tc := range []struct {
		src  string
		flag bool
		want int
	}{
		{cut, false, 0},
		{cut, true, 3},
		{full, true, 0},
	} {
		ExitDamaged, sawDamage = tc.flag, false
		dst := filepath.Join(dir, fmt.Sprintf("out%d.mkv", i))
		if got := exitStatus(t, func() { CmdSalvage([]string{tc.src, dst}) }); got != -1 {
			t.Fatalf("salvage %s: exit status %d, want it to return", filepath.Base(tc.src), got)
		}
		if got := DamagedStatus(); got != tc.want {
			t.Errorf("salvage %s, --exit-damaged %v: status %d, want %d", filepath.Base(tc.src), tc.flag, got, tc.want)
		}
	}
}

// TestJSONOutput: under --json an error is one JSON object on stderr carrying
// its exit status, and reindex prints its report as one JSON object.
func TestJSONOutput(t *testing.T) {
	JsonOutput = true
	t.Cleanup(func() { JsonOutput = false })
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.mkv")

	for _, tc := range []struct {
		name string
		run  func()
		want int
	}{
		{"bad usage", func() { CmdReindex(nil) }, 2},
		{"failure", func() { CmdReindex([]string{missing, filepath.Join(dir, "a.mkv")}) }, 1},
	} {
		code, stderr := exitStatusStderr(t, tc.run)
		var got struct {
			Error string `json:"error"`
			Exit  int    `json:"exit"`
		}
		line := strings.TrimSuffix(string(stderr), "\n")
		if err := json.Unmarshal([]byte(line), &got); err != nil || strings.ContainsAny(line, "\r\n") || got.Error == "" || got.Exit != tc.want || code != tc.want {
			t.Errorf("%s: exit %d, stderr %q (decode err %v), want {\"error\":...,\"exit\":%d}", tc.name, code, stderr, err, tc.want)
		}
	}

	src := filepath.Join("..", "..", "..", "internal", "testdata", "sample.mkv")
	dst := filepath.Join(dir, "out.mkv")
	out := captureStdout(t, func() { CmdReindex([]string{src, dst}) })
	var rep struct {
		Source string `json:"source"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(out, &rep); err != nil || rep.Source != src || rep.Output != dst {
		t.Errorf("reindex --json printed %q (decode err %v)", out, err)
	}
}

// exitStatusStderr is exitStatus also returning what fn wrote to stderr.
func exitStatusStderr(t *testing.T, fn func()) (int, []byte) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stderr
	os.Stderr = f
	code := func() (code int) {
		oldExit := osExit
		osExit = func(c int) { code = c; panic(errExited) }
		defer func() {
			osExit = oldExit
			if r := recover(); r != nil && r != errExited {
				panic(r)
			}
		}()
		code = -1
		fn()
		return code
	}()
	os.Stderr = old
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return code, b
}

// captureStdout returns what fn wrote to stdout.
func captureStdout(t *testing.T, fn func()) []byte {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = old }()
	fn()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return b
}
