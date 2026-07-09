package commands_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

func TestCmdFingerprint_TextOutput(t *testing.T) {
	out := capture(t, func() { commands.CmdFingerprint([]string{regfixMKV}) })
	if !strings.Contains(out, "Presentation:") || !strings.Contains(out, "track ") {
		t.Errorf("CmdFingerprint: expected a Presentation line and per-track lines\noutput:\n%s", out)
	}
}

func TestCmdFingerprint_JSON(t *testing.T) {
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	out := capture(t, func() { commands.CmdFingerprint([]string{regfixMKV}) })

	var fp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &fp); err != nil {
		t.Fatalf("CmdFingerprint JSON parse: %v\n%s", err, out)
	}
	if _, ok := fp["presentation"]; !ok {
		t.Errorf("CmdFingerprint JSON: expected a presentation field, got %v", fp)
	}
	tracks, ok := fp["tracks"].([]interface{})
	if !ok || len(tracks) == 0 {
		t.Errorf("CmdFingerprint JSON: expected a non-empty tracks list, got %v", fp["tracks"])
	}
}

func TestCmdFingerprint_MP4Rejected(t *testing.T) {
	var called bool
	capture(t, func() { called = runExpectingFatal(t, func() { commands.CmdFingerprint([]string{"video.mp4"}) }) })
	if !called {
		t.Error("CmdFingerprint on .mp4: expected Fatal (not supported yet)")
	}
}

func TestCmdFingerprint_NoArgs(t *testing.T) {
	var called bool
	capture(t, func() { called = runExpectingFatal(t, func() { commands.CmdFingerprint(nil) }) })
	if !called {
		t.Error("CmdFingerprint with no args: expected Fatal")
	}
}
