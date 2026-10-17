package commands

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestFatalErrNamesTheRepairCommand: the CLI, not the library, names the command that repairs a damaged file.
func TestFatalErrNamesTheRepairCommand(t *testing.T) {
	c := captureFatals(t)
	damage := fmt.Errorf("extract: %w", &reader.DamageError{Offset: 10, Resume: 20, Cause: errors.New("invalid VINT")})
	if got := c.run(func() { FatalErr(damage) }); !strings.Contains(got, "mkvgo reindex --resync") {
		t.Errorf("a damage refusal without the repair command: %q", got)
	}
	if got := c.run(func() { FatalErr(errors.New("plain")) }); got != "plain\n" {
		t.Errorf("another error changed: %q", got)
	}
}
