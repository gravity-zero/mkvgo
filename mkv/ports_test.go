package mkv

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFS_Nil_FallsBackToOS(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.txt")

	var fs *FS

	// DoMkdirAll on existing dir
	if err := fs.DoMkdirAll(tmp, 0o755); err != nil {
		t.Fatalf("DoMkdirAll: %v", err)
	}

	// DoWriteFile
	if err := fs.DoWriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("DoWriteFile: %v", err)
	}

	// DoStat
	info, err := fs.DoStat(path)
	if err != nil {
		t.Fatalf("DoStat: %v", err)
	}
	if info.Size() != 5 {
		t.Errorf("DoStat size = %d, want 5", info.Size())
	}

	// DoOpen
	rc, err := fs.DoOpen(path)
	if err != nil {
		t.Fatalf("DoOpen: %v", err)
	}
	buf := make([]byte, 5)
	n, _ := rc.Read(buf)
	rc.Close()
	if string(buf[:n]) != "hello" {
		t.Errorf("DoOpen read = %q, want %q", buf[:n], "hello")
	}

	// DoCreate
	path2 := filepath.Join(tmp, "test2.txt")
	wc, err := fs.DoCreate(path2)
	if err != nil {
		t.Fatalf("DoCreate: %v", err)
	}
	wc.Write([]byte("world"))
	wc.Close()
	data, _ := os.ReadFile(path2)
	if string(data) != "world" {
		t.Errorf("DoCreate wrote %q, want %q", data, "world")
	}

	// DoRemove
	if err := fs.DoRemove(path); err != nil {
		t.Fatalf("DoRemove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("DoRemove did not remove file")
	}
}

func TestFS_CustomFuncs(t *testing.T) {
	errCustom := errors.New("custom")

	fs := &FS{
		Open:      func(string) (ReadSeekCloser, error) { return nil, errCustom },
		Create:    func(string) (WriteSeekCloser, error) { return nil, errCustom },
		Stat:      func(string) (os.FileInfo, error) { return nil, errCustom },
		MkdirAll:  func(string, os.FileMode) error { return errCustom },
		WriteFile: func(string, []byte, os.FileMode) error { return errCustom },
		Remove:    func(string) error { return errCustom },
		OpenFile:  func(string, int, os.FileMode) (ReadWriteSeekCloser, error) { return nil, errCustom },
	}

	if _, err := fs.DoOpen("x"); !errors.Is(err, errCustom) {
		t.Errorf("DoOpen: got %v, want custom", err)
	}
	if _, err := fs.DoCreate("x"); !errors.Is(err, errCustom) {
		t.Errorf("DoCreate: got %v, want custom", err)
	}
	if _, err := fs.DoStat("x"); !errors.Is(err, errCustom) {
		t.Errorf("DoStat: got %v, want custom", err)
	}
	if err := fs.DoMkdirAll("x", 0o755); !errors.Is(err, errCustom) {
		t.Errorf("DoMkdirAll: got %v, want custom", err)
	}
	if err := fs.DoWriteFile("x", nil, 0o644); !errors.Is(err, errCustom) {
		t.Errorf("DoWriteFile: got %v, want custom", err)
	}
	if err := fs.DoRemove("x"); !errors.Is(err, errCustom) {
		t.Errorf("DoRemove: got %v, want custom", err)
	}
	if _, err := fs.DoOpenFile("x", 0, 0); !errors.Is(err, errCustom) {
		t.Errorf("DoOpenFile: got %v, want custom", err)
	}
}

func TestFS_NonNil_NilFields_FallsBackToOS(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.txt")

	fs := &FS{} // non-nil but all fields nil

	if err := fs.DoWriteFile(path, []byte("hi"), 0o644); err != nil {
		t.Fatalf("DoWriteFile: %v", err)
	}

	if _, err := fs.DoStat(path); err != nil {
		t.Fatalf("DoStat: %v", err)
	}

	if err := fs.DoRemove(path); err != nil {
		t.Fatalf("DoRemove: %v", err)
	}
}
