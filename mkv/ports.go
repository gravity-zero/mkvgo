package mkv

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
)

// ErrSourceChanged is returned by a read or a plan bound to one file once the
// path serves another version of it: open or plan the source again.
var ErrSourceChanged = errors.New("mkv: the source changed since it was opened or planned: open or plan it again")

// ErrMPEGTS is returned for an MPEG transport stream (or a Blu-ray M2TS) handed to any reader, whatever its name
// says: mkvgo does not read MPEG-TS. matroska.ErrMPEGTS and mp4.ErrMPEGTS are this error.
var ErrMPEGTS = errors.New("MPEG-TS detected: mkvgo does not read MPEG transport streams; remux the file to MP4 or Matroska first")

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

// RangeHinter is a reader that can be told the byte range [off, end) its next
// reads will cover, so a remote one fetches it with one request instead of a
// window at a time. A hint is advice: reads outside it work as before.
type RangeHinter interface{ HintRange(off, end int64) }

// ShareReads returns a context under which the readers an FS opens may serve
// each other the bytes one of them already fetched, without asking the source
// again: for a burst of reads of one version (a plan being built), never for
// reads that must notice a source replaced since. An FS that keeps nothing
// ignores it.
func ShareReads(ctx context.Context) context.Context {
	return WithReadShare(ctx, &ReadShare{})
}

// WithReadShare returns ctx under the scope s (opened by ShareReads): readers
// opened with it share with the others of s, and end when ctx does.
func WithReadShare(ctx context.Context, s *ReadShare) context.Context {
	return context.WithValue(ctx, readShareKey{}, s)
}

// ReadShare is the scope ShareReads opens: an FS keeps there, under a key of
// its own, what its readers share.
type ReadShare struct{ m sync.Map }

// LoadOrStore returns the value kept under key, storing v first when there is none.
func (s *ReadShare) LoadOrStore(key, v any) any {
	got, _ := s.m.LoadOrStore(key, v)
	return got
}

// ReadShareFrom returns the scope ShareReads opened on ctx, nil when none.
func ReadShareFrom(ctx context.Context) *ReadShare {
	s, _ := ctx.Value(readShareKey{}).(*ReadShare)
	return s
}

type readShareKey struct{}

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
