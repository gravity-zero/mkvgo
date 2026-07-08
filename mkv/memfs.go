package mkv

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// memfs.go — an in-memory FS implementation. Every operation the FS port
// exposes works on byte slices held in a map, so any mkvgo operation can run
// without touching a real filesystem: WebAssembly (no filesystem at all),
// tests, or servers assembling outputs to ship elsewhere.

// MemFS is an in-memory filesystem for the FS port. The zero value is not
// usable; create one with NewMemFS. Safe for concurrent use.
type MemFS struct {
	mu    sync.Mutex
	files map[string]*memFile
}

type memFile struct {
	data []byte
}

// memKey normalises a path to forward slashes so keys are independent of the
// host OS separator: a caller writing filepath.Join("hls", "init.mp4") gets
// "hls\\init.mp4" on Windows and "hls/init.mp4" elsewhere, but both must hit
// the same virtual file. The replacement is unconditional (not filepath.ToSlash,
// which only converts the current OS's separator) so MemFS behaves identically
// on every platform — its keys are URL-style resource paths, never real
// filenames containing a literal backslash. Matches Go's io/fs convention ("/").
func memKey(p string) string { return strings.ReplaceAll(p, "\\", "/") }

// NewMemFS returns an empty in-memory filesystem.
func NewMemFS() *MemFS {
	return &MemFS{files: map[string]*memFile{}}
}

// Put stores data under path (replacing any previous content).
func (m *MemFS) Put(path string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[memKey(path)] = &memFile{data: append([]byte(nil), data...)}
}

// Get returns the content stored under path, or nil when absent.
func (m *MemFS) Get(path string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f, ok := m.files[memKey(path)]; ok {
		return f.data
	}
	return nil
}

// Paths returns the stored paths, sorted.
func (m *MemFS) Paths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.files))
	for p := range m.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// FS returns the port wired to this in-memory filesystem, for any operation
// taking Options{FS: …} or an mp4.Options FS.
func (m *MemFS) FS() *FS {
	return &FS{
		Open: func(path string) (ReadSeekCloser, error) {
			m.mu.Lock()
			f, ok := m.files[memKey(path)]
			m.mu.Unlock()
			if !ok {
				return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
			}
			return &memReader{Reader: bytes.NewReader(f.data)}, nil
		},
		Create: func(path string) (WriteSeekCloser, error) {
			f := &memFile{}
			m.mu.Lock()
			m.files[memKey(path)] = f
			m.mu.Unlock()
			return &memWriter{f: f}, nil
		},
		OpenFile: func(path string, flag int, perm os.FileMode) (ReadWriteSeekCloser, error) {
			m.mu.Lock()
			f, ok := m.files[memKey(path)]
			m.mu.Unlock()
			if !ok {
				if flag&os.O_CREATE == 0 {
					return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
				}
				f = &memFile{}
				m.mu.Lock()
				m.files[memKey(path)] = f
				m.mu.Unlock()
			}
			return &memRW{f: f}, nil
		},
		Stat: func(path string) (os.FileInfo, error) {
			m.mu.Lock()
			f, ok := m.files[memKey(path)]
			m.mu.Unlock()
			if !ok {
				return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
			}
			return memInfo{name: memKey(path), size: int64(len(f.data))}, nil
		},
		MkdirAll:  func(string, os.FileMode) error { return nil }, // paths are flat keys
		WriteFile: func(path string, data []byte, _ os.FileMode) error { m.Put(path, data); return nil },
		Remove: func(path string) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			if _, ok := m.files[memKey(path)]; !ok {
				return &os.PathError{Op: "remove", Path: path, Err: os.ErrNotExist}
			}
			delete(m.files, memKey(path))
			return nil
		},
		Rename: func(oldpath, newpath string) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			f, ok := m.files[memKey(oldpath)]
			if !ok {
				return &os.PathError{Op: "rename", Path: oldpath, Err: os.ErrNotExist}
			}
			m.files[memKey(newpath)] = f
			delete(m.files, memKey(oldpath))
			return nil
		},
	}
}

type memReader struct{ *bytes.Reader }

func (r *memReader) Close() error { return nil }

// memWriter implements WriteSeekCloser over a growable byte slice.
type memWriter struct {
	f   *memFile
	pos int64
}

func (w *memWriter) Write(p []byte) (int, error) {
	end := w.pos + int64(len(p))
	if end > int64(len(w.f.data)) {
		grown := make([]byte, end)
		copy(grown, w.f.data)
		w.f.data = grown
	}
	copy(w.f.data[w.pos:end], p)
	w.pos = end
	return len(p), nil
}

func (w *memWriter) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = w.pos + offset
	case io.SeekEnd:
		next = int64(len(w.f.data)) + offset
	default:
		return 0, fmt.Errorf("memfs: bad whence %d", whence)
	}
	if next < 0 {
		return 0, fmt.Errorf("memfs: negative seek %d", next)
	}
	w.pos = next
	return next, nil
}

func (w *memWriter) Close() error { return nil }

// memRW adds reads on top of memWriter, for OpenFile (in-place edits).
type memRW struct {
	f   *memFile
	pos int64
}

func (rw *memRW) Read(p []byte) (int, error) {
	if rw.pos >= int64(len(rw.f.data)) {
		return 0, io.EOF
	}
	n := copy(p, rw.f.data[rw.pos:])
	rw.pos += int64(n)
	return n, nil
}

func (rw *memRW) Write(p []byte) (int, error) {
	end := rw.pos + int64(len(p))
	if end > int64(len(rw.f.data)) {
		grown := make([]byte, end)
		copy(grown, rw.f.data)
		rw.f.data = grown
	}
	copy(rw.f.data[rw.pos:end], p)
	rw.pos = end
	return len(p), nil
}

func (rw *memRW) Seek(offset int64, whence int) (int64, error) {
	w := memWriter{f: rw.f, pos: rw.pos}
	n, err := w.Seek(offset, whence)
	if err == nil {
		rw.pos = n
	}
	return n, err
}

func (rw *memRW) Close() error { return nil }

// Truncate resizes the file, mirroring os.File.Truncate so in-place operations
// (journal cleanup, rollback) work on MemFS too.
func (rw *memRW) Truncate(size int64) error {
	if size < 0 {
		return fmt.Errorf("memfs: negative truncate %d", size)
	}
	if size <= int64(len(rw.f.data)) {
		rw.f.data = rw.f.data[:size]
		return nil
	}
	grown := make([]byte, size)
	copy(grown, rw.f.data)
	rw.f.data = grown
	return nil
}

// memInfo is the minimal os.FileInfo Stat returns.
type memInfo struct {
	name string
	size int64
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() os.FileMode  { return 0o644 }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return false }
func (i memInfo) Sys() any           { return nil }
