package commands_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

func TestCmdAddRemoveAttachment(t *testing.T) {
	dir := t.TempDir()
	cover := filepath.Join(dir, "cover.jpg")
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}
	if err := os.WriteFile(cover, jpeg, 0o644); err != nil {
		t.Fatal(err)
	}

	withAtt := filepath.Join(dir, "with_att.mkv")
	out := capture(t, func() {
		commands.CmdAddAttachment([]string{regfixMKV, "-o", withAtt, cover})
	})
	if !strings.Contains(out, "image/jpeg") {
		t.Errorf("add-attachment: MIME not sniffed as image/jpeg\noutput: %s", out)
	}
	list := capture(t, func() { commands.CmdAttachments(withAtt) })
	if !strings.Contains(list, "cover.jpg") {
		t.Fatalf("attachment missing from output file:\n%s", list)
	}

	// Remove it by name.
	removed := filepath.Join(dir, "removed.mkv")
	capture(t, func() {
		commands.CmdRemoveAttachment([]string{withAtt, "-o", removed, "cover.jpg"})
	})
	list = capture(t, func() { commands.CmdAttachments(removed) })
	if strings.Contains(list, "cover.jpg") {
		t.Errorf("attachment still present after remove-attachment:\n%s", list)
	}

	// Unknown target fails BEFORE writing any output. The exit hook panics so
	// execution stops at Fatal, as os.Exit would.
	exitCode := -1
	restore := commands.SetExit(func(c int) { exitCode = c; panic("exit") })
	defer restore()
	ghostOut := filepath.Join(dir, "ghost.mkv")
	func() {
		defer func() { _ = recover() }()
		commands.CmdRemoveAttachment([]string{withAtt, "-o", ghostOut, "nope.bin"})
	}()
	if exitCode != 1 {
		t.Errorf("remove-attachment with unknown target: exit = %d, want 1", exitCode)
	}
	if _, err := os.Stat(ghostOut); err == nil {
		t.Errorf("output file written despite unknown attachment target")
	}
}
