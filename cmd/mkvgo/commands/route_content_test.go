package commands_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

// renamed copies src under a name ending in ext.
func renamed(t *testing.T, src, ext string) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "mislabeled"+ext)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRoutesByContent: the bytes decide the engine, not the extension - a
// Matroska named .mp4 is read as a Matroska, an MP4 named .mkv as an MP4.
func TestRoutesByContent(t *testing.T) {
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })

	mkvAsMP4 := renamed(t, richMKV(t), ".mp4")
	mp4AsMKV := renamed(t, mp4Copy(t, "bframes_aac.mp4"), ".mkv")

	for name, run := range map[string]func(){
		"info, Matroska named .mp4":      func() { commands.CmdInfo(mkvAsMP4) },
		"keyframes, Matroska named .mp4": func() { commands.CmdKeyframes(mkvAsMP4) },
		"keyframes, MP4 named .mkv":      func() { commands.CmdKeyframes(mp4AsMKV) },
		"analyze, Matroska named .mp4":   func() { commands.CmdAnalyze([]string{mkvAsMP4}) },
	} {
		t.Run(name, func(t *testing.T) {
			exited := false
			restore := commands.SetExit(func(int) { panic("exit") })
			defer restore()
			out := capture(t, func() {
				defer func() {
					if recover() != nil {
						exited = true
					}
				}()
				run()
			})
			if exited || strings.TrimSpace(out) == "" {
				t.Errorf("exited %v, report %q", exited, out)
			}
		})
	}
	mustFatal(t, func() { commands.CmdAnalyze([]string{mp4AsMKV}) })
}
