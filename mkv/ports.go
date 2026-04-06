package mkv

import (
	"io"
	"os"
)

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
}

// Default FS operations — fallback to os package.

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
