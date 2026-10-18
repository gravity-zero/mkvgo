package commands_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

// TestForceNeverWritesOverAnInput pins that -f lets a command replace an existing file, never the file it reads:
// add-attachment -f -o a.mkv a.mkv truncated its source before reading it.
func TestForceNeverWritesOverAnInput(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.mkv")
	other := filepath.Join(dir, "b.mkv")
	content := []byte("not touched")
	for _, p := range []string{src, other} {
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commands.Force = true
	t.Cleanup(func() { commands.Force = false })
	cases := map[string]func(){
		"reindex":               func() { commands.CmdReindex([]string{src, src}) },
		"salvage":               func() { commands.CmdSalvage([]string{src, src}) },
		"to-webm":               func() { commands.CmdToWebM([]string{src, src}) },
		"from-mp4":              func() { commands.CmdFromMP4([]string{src, src}) },
		"add-attachment":        func() { commands.CmdAddAttachment([]string{"-o", src, src, other}) },
		"remove-attachment":     func() { commands.CmdRemoveAttachment([]string{"-o", src, src, "1"}) },
		"concat-segment":        func() { commands.CmdConcatSegment([]string{"-o", src, "1", src, other}) },
		"watermark-segment":     func() { commands.CmdWatermarkSegment([]string{"-o", src, src, other, "1"}) },
		"rollback":              func() { commands.CmdRollback([]string{src, other, src}) },
		"extract-attachment":    func() { commands.CmdExtractAttachment([]string{src, "1", "-o", src}) },
		"extract-subtitle":      func() { commands.CmdExtractSubtitle([]string{src, "-t", "2", "-o", src}) },
		"subtitle-index":        func() { commands.CmdSubtitleIndex([]string{src, "-o", src}) },
		"extract-chapters":      func() { commands.CmdExtractChapters([]string{"-o", src, src}) },
		"hls-segment":           func() { commands.CmdHLSSegment([]string{"-o", src, src, "1"}) },
		"reindex-inplace delta": func() { commands.CmdReindexInPlace([]string{"--rollback-delta", src, src}) },
		"reindex delta":         func() { commands.CmdReindex([]string{"--rollback-delta", src, src, other}) },
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			exited := false
			restore := commands.SetExit(func(int) { exited = true; panic("exit") })
			func() {
				defer func() { _ = recover() }()
				run()
			}()
			restore()
			if !exited {
				t.Errorf("%s with -f and its input as output did not refuse", name)
			}
			if got, _ := os.ReadFile(src); !bytes.Equal(got, content) {
				t.Fatalf("%s changed its input", name)
			}
		})
	}
}
