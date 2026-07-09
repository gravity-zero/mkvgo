package commands_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

func TestCmdIngest_TextOutput(t *testing.T) {
	out := capture(t, func() { commands.CmdIngest([]string{regfixMKV}) })
	if !strings.Contains(out, "Strategy:") || !strings.Contains(out, "Target:") {
		t.Errorf("CmdIngest: expected a Strategy/Target summary\noutput:\n%s", out)
	}
}

func TestCmdIngest_JSON(t *testing.T) {
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	out := capture(t, func() { commands.CmdIngest([]string{regfixMKV}) })

	var plan map[string]interface{}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("CmdIngest JSON parse: %v\n%s", err, out)
	}
	if plan["Strategy"] == nil {
		t.Errorf("CmdIngest JSON: expected a Strategy field, got %v", plan)
	}
}

func TestCmdIngest_UnknownTarget(t *testing.T) {
	var called bool
	capture(t, func() {
		called = runExpectingFatal(t, func() { commands.CmdIngest([]string{"-target", "nonexistent", regfixMKV}) })
	})
	if !called {
		t.Error("CmdIngest with an unknown target: expected Fatal")
	}
}

func TestCmdIngest_NoArgs(t *testing.T) {
	var called bool
	capture(t, func() { called = runExpectingFatal(t, func() { commands.CmdIngest(nil) }) })
	if !called {
		t.Error("CmdIngest with no args: expected Fatal")
	}
}
