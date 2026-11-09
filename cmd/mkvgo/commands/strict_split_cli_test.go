package commands_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

// --strict is global: a command other than the remuxes that refuses a dropped track names the flag too, not the
// library's Options field (split printed "which Options.Strict refuses").
func TestStrictRefusalNamesTheFlagOnEveryCommand(t *testing.T) {
	commands.Strict = true
	t.Cleanup(func() { commands.Strict = false })
	msg := fatalMessage(t, 1, func() {
		commands.CmdSplit([]string{"../../../internal/testdata/tmcd.mov", "-o", filepath.Join(t.TempDir(), "parts"), "-every", "1000"})
	})
	if !strings.Contains(msg, "--strict") || strings.Contains(msg, "Options.") {
		t.Errorf("split --strict: %q, want the track refused by --strict", msg)
	}
}
