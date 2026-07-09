package commands_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

func TestCmdAnalyze_TextOutput(t *testing.T) {
	out := capture(t, func() { commands.CmdAnalyze([]string{regfixMKV}) })
	if !strings.Contains(out, "Duration:") || !strings.Contains(out, "Track ") {
		t.Errorf("CmdAnalyze: expected a duration summary and per-track lines\noutput:\n%s", out)
	}
}

func TestCmdAnalyze_JSON(t *testing.T) {
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	out := capture(t, func() { commands.CmdAnalyze([]string{regfixMKV}) })

	var report map[string]interface{}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("CmdAnalyze JSON parse: %v\n%s", err, out)
	}
	tracks, ok := report["tracks"].([]interface{})
	if !ok || len(tracks) == 0 {
		t.Errorf("CmdAnalyze JSON: expected a non-empty tracks list, got %v", report["tracks"])
	}
}

// runExpectingFatal runs fn with a panicking exit hook (Fatal is meant to
// terminate the process; CmdAnalyze does not guard against continuing past
// it) and reports whether Fatal fired.
func runExpectingFatal(t *testing.T, fn func()) (called bool) {
	t.Helper()
	restore := commands.SetExit(func(int) { called = true; panic("exit") })
	defer func() {
		restore()
		_ = recover()
	}()
	fn()
	return called
}

func TestCmdAnalyze_MP4Rejected(t *testing.T) {
	var called bool
	capture(t, func() { called = runExpectingFatal(t, func() { commands.CmdAnalyze([]string{"video.mp4"}) }) })
	if !called {
		t.Error("CmdAnalyze on .mp4: expected Fatal (not supported yet)")
	}
}

func TestCmdAnalyze_NoArgs(t *testing.T) {
	var called bool
	capture(t, func() { called = runExpectingFatal(t, func() { commands.CmdAnalyze(nil) }) })
	if !called {
		t.Error("CmdAnalyze with no args: expected Fatal")
	}
}
