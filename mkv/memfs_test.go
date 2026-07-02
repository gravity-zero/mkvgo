package mkv

import (
	"io"
	"os"
	"testing"
)

// The in-memory FS supports the full port surface: create/write/seek/read
// round-trip, stat, in-place read-write, remove, and not-found errors.
func TestMemFS(t *testing.T) {
	m := NewMemFS()
	fs := m.FS()

	w, err := fs.DoCreate("a/b.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	// Backpatch like MKVWriter does: seek back, overwrite, seek past the end.
	if _, err := w.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("HELLO")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Seek(3, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("!")); err != nil {
		t.Fatal(err)
	}
	w.Close()

	want := "HELLO world\x00\x00\x00!"
	if got := string(m.Get("a/b.mkv")); got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	st, err := fs.DoStat("a/b.mkv")
	if err != nil || st.Size() != int64(len(want)) {
		t.Fatalf("stat = %v/%v, want size %d", st, err, len(want))
	}

	r, err := fs.DoOpen("a/b.mkv")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != want {
		t.Fatalf("read back = %q", got)
	}

	rw, err := fs.DoOpenFile("a/b.mkv", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Seek(6, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Write([]byte("WORLD")); err != nil {
		t.Fatal(err)
	}
	rw.Close()
	if got := string(m.Get("a/b.mkv")); got[6:11] != "WORLD" {
		t.Fatalf("in-place edit = %q", got)
	}

	if _, err := fs.DoOpen("missing"); !os.IsNotExist(err) {
		t.Errorf("open missing: err = %v, want not-exist", err)
	}
	if err := fs.DoRemove("a/b.mkv"); err != nil {
		t.Fatal(err)
	}
	if len(m.Paths()) != 0 {
		t.Errorf("paths after remove = %v", m.Paths())
	}
}
