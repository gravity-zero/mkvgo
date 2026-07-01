package commands_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

func TestGuardOverwrite(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "out.mkv")
	if err := os.WriteFile(existing, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("existing file refused", func(t *testing.T) {
		exited := false
		restore := commands.SetExit(func(int) { exited = true; panic("exit") })
		defer func() {
			restore()
			_ = recover()
			if !exited {
				t.Errorf("GuardOverwrite on an existing file: expected Fatal, got none")
			}
		}()
		commands.GuardOverwrite(existing)
		t.Errorf("GuardOverwrite returned without refusing the existing file")
	})

	t.Run("force allows overwrite", func(t *testing.T) {
		commands.Force = true
		t.Cleanup(func() { commands.Force = false })
		commands.GuardOverwrite(existing) // must not exit
	})

	t.Run("new path passes", func(t *testing.T) {
		commands.GuardOverwrite(filepath.Join(dir, "new.mkv"))
	})

	t.Run("stdin/stdout dash passes", func(t *testing.T) {
		commands.GuardOverwrite("-")
	})
}
