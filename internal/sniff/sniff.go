// Package sniff reads a source's first bytes to tell its container, at no cost to the read that follows: a remote
// reader's response is handed on to the engine instead of a second request.
package sniff

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/mkv"
)

// Head reads path's first n bytes through fs (nil = the OS). A shorter source returns what it holds. A remote
// reader (httpfs, s3fs) is not closed: the returned FS hands it to the first open of path, its head served again,
// so the engine reads on from the response the sniff opened. Any other reader is closed and the returned FS is fs.
// An error says the source could not be read, not that it is of some other kind.
func Head(ctx context.Context, path string, fs *mkv.FS, n int) ([]byte, *mkv.FS, error) {
	f, err := fs.DoOpenContext(ctx, path)
	if err != nil {
		return nil, fs, err
	}
	head := make([]byte, n)
	k, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		f.Close()
		return nil, fs, fmt.Errorf("read the head of %s: %w", shown.Path(path), err)
	}
	head = head[:k]
	r, ok := f.(remoteReader)
	if !ok {
		f.Close()
		return head, fs, nil
	}
	replay := &headReplay{remoteReader: r, head: head}
	var once sync.Once
	first := func(p string) (mkv.ReadSeekCloser, bool) {
		var h mkv.ReadSeekCloser
		if p == path {
			once.Do(func() { h = replay })
		}
		return h, h != nil
	}
	out := mkv.FS{}
	if fs != nil {
		out = *fs
	}
	out.Open = func(p string) (mkv.ReadSeekCloser, error) {
		if h, ok := first(p); ok {
			return h, nil
		}
		return fs.DoOpen(p)
	}
	out.OpenContext = func(ctx context.Context, p string) (mkv.ReadSeekCloser, error) {
		if h, ok := first(p); ok {
			return h, nil
		}
		return fs.DoOpenContext(ctx, p)
	}
	return head, &out, nil
}

// remoteReader is what an httpfs/s3fs reader offers beyond reading: headReplay keeps it so the engines still pin,
// hint and read ranges through it.
type remoteReader interface {
	mkv.ReadSeekCloser
	io.ReaderAt
	mkv.PinnedReader
	mkv.RangeHinter
	Free(off, n int64) bool
	Stat() (os.FileInfo, error)
}

// headReplay serves the sniffed head again, then reads on from the response the sniff left open.
type headReplay struct {
	remoteReader
	head []byte
	pos  int64
}

func (h *headReplay) Read(p []byte) (int, error) {
	if h.pos < int64(len(h.head)) {
		n := copy(p, h.head[h.pos:])
		h.pos += int64(n)
		return n, nil
	}
	if _, err := h.remoteReader.Seek(h.pos, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := h.remoteReader.Read(p)
	h.pos += int64(n)
	return n, err
}

func (h *headReplay) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		h.pos = offset
	case io.SeekCurrent:
		h.pos += offset
	default:
		next, err := h.remoteReader.Seek(offset, whence)
		if err != nil {
			return 0, err
		}
		h.pos = next
	}
	if h.pos < 0 {
		return 0, fmt.Errorf("negative seek %d", h.pos)
	}
	return h.pos, nil
}
