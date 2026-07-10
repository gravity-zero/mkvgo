package commands_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/mp4"
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

// CmdFingerprint accepts an MP4 (via an internal remux to a temporary
// Matroska file) and produces the SAME Presentation hash as the MKV it came
// from, since RemuxToMP4 copies sample bytes verbatim.
func TestCmdFingerprint_MP4(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := mp4.RemuxToMP4(context.Background(), regfixMKV, out); err != nil {
		t.Fatal(err)
	}

	mkvOut := capture(t, func() { commands.CmdFingerprint([]string{regfixMKV}) })
	mp4Out := capture(t, func() { commands.CmdFingerprint([]string{out}) })

	presentation := func(s string) string {
		for _, line := range strings.Split(s, "\n") {
			if p, ok := strings.CutPrefix(line, "Presentation: "); ok {
				return p
			}
		}
		return ""
	}
	mkvP, mp4P := presentation(mkvOut), presentation(mp4Out)
	if mkvP == "" || mkvP != mp4P {
		t.Errorf("CmdFingerprint Presentation differs across containers: mkv=%q mp4=%q", mkvP, mp4P)
	}
}

func TestCmdFingerprint_NoArgs(t *testing.T) {
	var called bool
	capture(t, func() { called = runExpectingFatal(t, func() { commands.CmdFingerprint(nil) }) })
	if !called {
		t.Error("CmdFingerprint with no args: expected Fatal")
	}
}
