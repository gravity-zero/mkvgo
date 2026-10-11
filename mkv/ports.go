package mkv

import (
	"context"
	"errors"
	"io"
	"os"
)

// ErrSourceChanged is returned by a read or a plan bound to one file once the
// path serves another version of it: open or plan the source again.
var ErrSourceChanged = errors.New("mkv: the source changed since it was opened or planned: open or plan it again")

// FileIdentity is what a FileInfo may offer beyond size and mtime: a token
// for the version of the file (an ETag, a version id, a Last-Modified date).
// Strong reports a token that changes with every change of the bytes (a
// strong ETag, a version id); a timestamp is not, two versions written within
// its resolution share it. Only a strong identity rules out mixing versions.
type FileIdentity interface {
	Identity() string
	Strong() bool
}

// PinnedReader is a reader that can be bound to one identity (FileIdentity)
// and then fails every read of another version with ErrSourceChanged.
type PinnedReader interface{ Pin(identity string) }

// ReadSeekCloser combines io.ReadSeeker and io.Closer.
type ReadSeekCloser interface {
	io.ReadSeeker
	io.Closer
}

// WriteSeekCloser combines io.WriteSeeker and io.Closer.
type WriteSeekCloser interface {
	io.WriteSeeker
	io.Closer
}

// ReadWriteSeekCloser combines io.ReadWriteSeeker and io.Closer.
type ReadWriteSeekCloser interface {
	io.ReadSeeker
	io.Writer
	io.Closer
}

// FS abstracts filesystem operations. Nil means use the real OS.
// Callers can provide implementations backed by S3, HTTP, or in-memory buffers.
type FS struct {
	Open      func(path string) (ReadSeekCloser, error)
	Create    func(path string) (WriteSeekCloser, error)
	OpenFile  func(path string, flag int, perm os.FileMode) (ReadWriteSeekCloser, error)
	Stat      func(path string) (os.FileInfo, error)
	MkdirAll  func(path string, perm os.FileMode) error
	WriteFile func(path string, data []byte, perm os.FileMode) error
	Remove    func(path string) error
	Rename    func(oldpath, newpath string) error
	// OpenContext, when set, opens a reader whose reads end when ctx does
	// (a network request in flight is abandoned); DoOpenContext falls back to Open.
	OpenContext func(ctx context.Context, path string) (ReadSeekCloser, error)
}

// Default FS operations - fallback to os package.

// DoOpenContext opens path for reads bound to ctx (OpenContext), else as DoOpen.
func (fs *FS) DoOpenContext(ctx context.Context, path string) (ReadSeekCloser, error) {
	if fs != nil && fs.OpenContext != nil {
		return fs.OpenContext(ctx, path)
	}
	return fs.DoOpen(path)
}

func (fs *FS) DoOpen(path string) (ReadSeekCloser, error) {
	if fs != nil && fs.Open != nil {
		return fs.Open(path)
	}
	return os.Open(path)
}

func (fs *FS) DoCreate(path string) (WriteSeekCloser, error) {
	if fs != nil && fs.Create != nil {
		return fs.Create(path)
	}
	return os.Create(path)
}

func (fs *FS) DoOpenFile(path string, flag int, perm os.FileMode) (ReadWriteSeekCloser, error) {
	if fs != nil && fs.OpenFile != nil {
		return fs.OpenFile(path, flag, perm)
	}
	return os.OpenFile(path, flag, perm)
}

func (fs *FS) DoStat(path string) (os.FileInfo, error) {
	if fs != nil && fs.Stat != nil {
		return fs.Stat(path)
	}
	return os.Stat(path)
}

func (fs *FS) DoMkdirAll(path string, perm os.FileMode) error {
	if fs != nil && fs.MkdirAll != nil {
		return fs.MkdirAll(path, perm)
	}
	return os.MkdirAll(path, perm)
}

func (fs *FS) DoWriteFile(path string, data []byte, perm os.FileMode) error {
	if fs != nil && fs.WriteFile != nil {
		return fs.WriteFile(path, data, perm)
	}
	return os.WriteFile(path, data, perm)
}

func (fs *FS) DoRemove(path string) error {
	if fs != nil && fs.Remove != nil {
		return fs.Remove(path)
	}
	return os.Remove(path)
}

func (fs *FS) DoRename(oldpath, newpath string) error {
	if fs != nil && fs.Rename != nil {
		return fs.Rename(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}
