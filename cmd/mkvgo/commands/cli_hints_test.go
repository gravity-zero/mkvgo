package commands_test

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

// TestFlagWordsNameTheFlag pins that a library refusal naming Options.SkipUnsupported names the flag on the command
// line, its sentinel kept.
func TestFlagWordsNameTheFlag(t *testing.T) {
	base := fmt.Errorf("wrapped: %w", os.ErrInvalid)
	err := commands.FlagWords(fmt.Errorf("mp4: track 1 (alac): no Matroska form (set Options.SkipUnsupported to drop it): %w", base))
	if msg := err.Error(); strings.Contains(msg, "Options.") || !strings.Contains(msg, "(pass --skip-unsupported to drop it)") {
		t.Errorf("message %q", msg)
	}
	if !strings.Contains(commands.FlagWords(fmt.Errorf("re-encode it, or set Options.SkipUnsupported to drop the track")).Error(), "or pass --skip-unsupported to drop the track") {
		t.Error("the second wording is not translated")
	}
	if !errors.Is(err, os.ErrInvalid) {
		t.Error("the wrapped error is lost")
	}
}

// TestEditInPlaceRefusalNamesTheRewrite pins that edit-inplace, refused for want of room, names the command that
// rewrites the file.
func TestEditInPlaceRefusalNamesTheRewrite(t *testing.T) {
	path := richMKV(t)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0xEC, 0x81, 0x00}); err != nil { // bytes after the Segment: nowhere to move to
		t.Fatal(err)
	}
	f.Close()
	msg := fatalMessage(t, 1, func() {
		commands.CmdEditInPlace([]string{path, fmt.Sprintf(`{"title":%q}`, strings.Repeat("t", 70000))})
	})
	if !strings.Contains(msg, "use full rewrite instead") || !strings.Contains(msg, "mkvgo edit "+path+" -o <out>") {
		t.Errorf("refusal %q: want the rewrite command", msg)
	}
}
