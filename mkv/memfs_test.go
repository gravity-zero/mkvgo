package mkv

import (
	"io"
	"os"
	"runtime"
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

// Keys are separator-independent: on Windows the packager writes paths joined
// with filepath.Join ("hls\\init.mp4"), but consumers read them with forward
// slashes ("hls/init.mp4"). Both must reach the same file (io/fs convention).
func TestMemFSSeparatorAgnostic(t *testing.T) {
	m := NewMemFS()
	// Store the way a Windows filepath.Join would, read the way a URL-style
	// consumer does - and vice versa.
	m.Put("hls\\init.mp4", []byte("INIT"))
	if got := m.Get("hls/init.mp4"); string(got) != "INIT" {
		t.Errorf("Get with '/' after Put with '\\\\' = %q, want INIT", got)
	}
	fs := m.FS()
	w, _ := fs.DoCreate("hls\\seg00001.m4s")
	w.Write([]byte("SEG"))
	w.Close()
	r, err := fs.DoOpen("hls/seg00001.m4s")
	if err != nil {
		t.Fatalf("open across separators: %v", err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "SEG" {
		t.Errorf("read across separators = %q, want SEG", got)
	}
	// Paths are reported normalised, so a consumer's forward-slash lookup works.
	for _, p := range m.Paths() {
		if filepathHasBackslash(p) {
			t.Errorf("Paths() returned a non-normalised key: %q", p)
		}
	}
}

func filepathHasBackslash(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			return true
		}
	}
	return false
}

// TestMemFSWriteGrowsAmortized: a file written piece by piece grows like append;
// it was reallocated and copied whole on every write that extended it, which
// made an in-memory remux quadratic in its output size.
func TestMemFSWriteGrowsAmortized(t *testing.T) {
	fs := NewMemFS().FS()
	w, err := fs.Create("out")
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64<<10)
	const total = 16 << 20
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for n := 0; n < total; n += len(chunk) {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 4*total {
		t.Errorf("writing %d bytes allocated %d, want <= %d", total, alloc, 4*total)
	}
}

// TestMemFSGapAfterTruncateIsZero: bytes a truncate dropped never come back
// when the file is extended past them again.
func TestMemFSGapAfterTruncateIsZero(t *testing.T) {
	fs := NewMemFS().FS()
	rw, err := fs.OpenFile("f", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Write([]byte("abcdefgh")); err != nil {
		t.Fatal(err)
	}
	if err := rw.(interface{ Truncate(int64) error }).Truncate(2); err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Seek(6, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Write([]byte("XY")); err != nil {
		t.Fatal(err)
	}
	w, _ := fs.Create("g")
	if _, err := w.Write([]byte("0123")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Seek(6, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("Z")); err != nil {
		t.Fatal(err)
	}
	r, _ := fs.Open("f")
	got, _ := io.ReadAll(r)
	if string(got) != "ab\x00\x00\x00\x00XY" {
		t.Errorf("after truncate and extend: %q, want %q", got, "ab\x00\x00\x00\x00XY")
	}
	r, _ = fs.Open("g")
	if got, _ = io.ReadAll(r); string(got) != "0123\x00\x00Z" {
		t.Errorf("write past the end: %q, want %q", got, "0123\x00\x00Z")
	}
}

// TestMemFSReserve: a reserved output is written without growing.
func TestMemFSReserve(t *testing.T) {
	m := NewMemFS()
	const total = 16 << 20
	m.Reserve("out", total)
	w, err := m.FS().Create("out")
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64<<10)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for n := 0; n < total; n += len(chunk) {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 1<<20 {
		t.Errorf("writing a reserved %d-byte file allocated %d bytes", total, alloc)
	}
	if len(m.Get("out")) != total {
		t.Errorf("file is %d bytes, want %d", len(m.Get("out")), total)
	}
}
