package commands_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/mp4"
)

// capture runs fn with os.Stdout redirected and returns what it wrote.
func capture(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out)
}

// TestCLIKeyframes checks the keyframes command works for both MKV (Cues) and MP4.
func TestCLIKeyframes(t *testing.T) {
	dir := t.TempDir()
	mp4Path := filepath.Join(dir, "x.mp4")
	if err := mp4.RemuxToMP4(context.Background(), regfixMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}

	for _, path := range []string{regfixMKV, mp4Path} {
		commands.JsonOutput = true
		out := capture(t, func() { commands.CmdKeyframes(path) })
		commands.JsonOutput = false

		var ks []int64
		if err := json.Unmarshal([]byte(out), &ks); err != nil {
			t.Fatalf("keyframes JSON for %s: %v (%q)", path, err, out)
		}
		if len(ks) == 0 || ks[0] != 0 {
			t.Errorf("keyframes for %s = %v, want a non-empty list starting at 0", path, ks)
		}
	}
}

// TestCLIProbeMP4 checks the probe command reads an MP4 and reports keyframes and
// dropped tracks.
func TestCLIProbeMP4(t *testing.T) {
	dir := t.TempDir()
	mp4Path := filepath.Join(dir, "x.mp4")
	if err := mp4.RemuxToMP4(context.Background(), regfixMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	out := capture(t, func() { commands.CmdProbe(mp4Path) })
	for _, want := range []string{"Tracks (3)", "Keyframes:"} {
		if !strings.Contains(out, want) {
			t.Errorf("probe MP4 output missing %q:\n%s", want, out)
		}
	}
	// The QuickTime chapter track must NOT be surfaced as a dropped track.
	if strings.Contains(out, "Dropped tracks") {
		t.Errorf("chapter track wrongly reported as dropped:\n%s", out)
	}
}

// TestCLIToVTT checks the external-sidecar conversion command.
func TestCLIToVTT(t *testing.T) {
	dir := t.TempDir()
	srt := filepath.Join(dir, "s.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:02,000\nHi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vtt := filepath.Join(dir, "s.vtt")
	capture(t, func() { commands.CmdToVTT([]string{srt, "-o", vtt}) })

	data, err := os.ReadFile(vtt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "WEBVTT") || !strings.Contains(string(data), "00:00:01.000 --> 00:00:02.000\nHi") {
		t.Errorf("to-vtt output:\n%s", data)
	}
}
