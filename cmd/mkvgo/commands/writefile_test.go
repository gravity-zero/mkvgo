package commands

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileFromFailureTouchesNothing: a write that fails leaves no file
// under the output name, and leaves a file already there as it was.
func TestWriteFileFromFailureTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	refuse := func(w io.Writer) error {
		_, _ = w.Write([]byte("partial"))
		return errors.New("refused")
	}

	fresh := filepath.Join(dir, "fresh.vtt")
	if err := writeFileFrom(fresh, refuse); err == nil {
		t.Fatal("want the write error returned")
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Errorf("a failed write left %s behind (stat: %v)", fresh, err)
	}

	existing := filepath.Join(dir, "existing.vtt")
	if err := os.WriteFile(existing, []byte("an earlier result"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileFrom(existing, refuse); err == nil {
		t.Fatal("want the write error returned")
	}
	if got, _ := os.ReadFile(existing); string(got) != "an earlier result" {
		t.Errorf("a failed write changed the existing file to %q", got)
	}

	if err := writeFileFrom(fresh, func(w io.Writer) error { _, err := w.Write([]byte("WEBVTT\n")); return err }); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(fresh); string(got) != "WEBVTT\n" {
		t.Errorf("a successful write stored %q", got)
	}
}
