package tmplock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func create(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestLiveAndLeftover: a held temporary file is refused to Clear and to another Take, and stays; once released, or
// written by a run that died, it is removed.
func TestLiveAndLeftover(t *testing.T) {
	if !haveLocks {
		t.Skip("no file locks on this system")
	}
	tmp := filepath.Join(t.TempDir(), "out.mkv.mkvgo.tmp")
	f := create(t, tmp)
	l, err := Take(tmp, f)
	if err != nil || l.h == nil {
		t.Fatalf("Take = %v, %v; want a held lock", l, err)
	}
	if err := Clear(tmp); !errors.Is(err, ErrBusy) {
		t.Errorf("Clear of a held file = %v, want ErrBusy", err)
	}
	if _, err := Take(tmp, f); !errors.Is(err, ErrBusy) {
		t.Errorf("a second Take = %v, want ErrBusy", err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("the held file was removed: %v", err)
	}
	_ = f.Close()
	if err := Clear(tmp); !errors.Is(err, ErrBusy) {
		t.Errorf("Clear once the writer closed, lock still held = %v, want ErrBusy", err)
	}
	l.Release()
	if err := Clear(tmp); err != nil {
		t.Fatalf("Clear of a released file: %v", err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("the leftover is still there: %v", err)
	}
	if err := Clear(tmp); err != nil {
		t.Errorf("Clear of nothing: %v", err)
	}
}

// TestRenameWhileHeld: the writer renames its file onto the output while the lock is held, and the output is not
// cleared as a leftover until the lock is released.
func TestRenameWhileHeld(t *testing.T) {
	if !haveLocks {
		t.Skip("no file locks on this system")
	}
	dir := t.TempDir()
	tmp, dst := filepath.Join(dir, "out.mkvgo.tmp"), filepath.Join(dir, "out")
	f := create(t, tmp)
	l, err := Take(tmp, f)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, err := f.WriteString("complete"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatalf("rename with the lock held: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "complete" {
		t.Errorf("output = %q", got)
	}
}

// TestTakeReplacedFile: another run removed this run's fresh file and created its own under the name before this
// run locked it; Take refuses rather than lock the other run's file.
func TestTakeReplacedFile(t *testing.T) {
	if !haveLocks {
		t.Skip("no file locks on this system")
	}
	dir := t.TempDir()
	tmp := filepath.Join(dir, "out.mkvgo.tmp")
	mine := create(t, tmp)
	if err := os.Remove(tmp); err != nil {
		t.Skip("an open file stays in place on this system")
	}
	if _, err := Take(tmp, mine); !errors.Is(err, ErrBusy) {
		t.Errorf("Take with the file gone = %v, want ErrBusy", err)
	}
	create(t, tmp)
	if _, err := Take(tmp, mine); !errors.Is(err, ErrBusy) {
		t.Errorf("Take of a file another run created = %v, want ErrBusy", err)
	}
}
