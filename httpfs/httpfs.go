// Package httpfs implements the mkv.FS port over HTTP(S) Range requests, so
// any read-only mkvgo operation works on a remote URL as a path. Combined with
// the head-only probe, inspecting a remote file costs a few ranged kilobytes —
// indexing a media library on S3/HTTP transfers no full file.
//
//	c, err := matroska.OpenMetaWithFS(ctx, url, httpfs.New().Port())
//
// Reads are served from a cached window (default 512 KiB) fetched with
// `Range: bytes=…`, so a parser's many small reads don't each become a
// request. The server must honour Range requests (respond 206); one that
// replies 200 to a ranged request gets an explicit error rather than a silent
// full download. The FS is read-only: write operations return an error.
package httpfs

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

const defaultWindow = 512 << 10

// Options configures New. The zero value uses http.DefaultClient and a
// 512 KiB read window.
type Options struct {
	// Client is the HTTP client to use; nil means http.DefaultClient.
	Client *http.Client
	// WindowSize is the ranged-fetch granularity in bytes; <= 0 means 512 KiB.
	// Smaller windows transfer less for sparse access; larger ones make fewer
	// requests for sequential access.
	WindowSize int
	// Header is added to every request (e.g. Authorization).
	Header http.Header
}

// FS is the HTTP-backed filesystem. It also counts the bytes fetched, which
// tests and cost-conscious callers can read back.
type FS struct {
	opts    Options
	fetched atomic.Int64
}

// New returns an HTTP Range-backed FS. Paths passed to the returned port must
// be absolute http:// or https:// URLs.
func New(opts ...Options) *FS {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Client == nil {
		o.Client = http.DefaultClient
	}
	if o.WindowSize <= 0 {
		o.WindowSize = defaultWindow
	}
	return &FS{opts: o}
}

// BytesFetched returns the total bytes transferred so far across all readers.
func (f *FS) BytesFetched() int64 { return f.fetched.Load() }

// Port returns the mkv.FS wired to this HTTP filesystem.
func (f *FS) Port() *mkv.FS {
	roErr := func(op string) error { return fmt.Errorf("httpfs: %s: read-only filesystem", op) }
	return &mkv.FS{
		Open: func(url string) (mkv.ReadSeekCloser, error) {
			if !IsURL(url) {
				return nil, fmt.Errorf("httpfs: not an http(s) URL: %s", url)
			}
			return &reader{fs: f, url: url, size: -1}, nil
		},
		Stat: func(url string) (os.FileInfo, error) {
			r := &reader{fs: f, url: url, size: -1}
			if err := r.fetch(0); err != nil {
				return nil, err
			}
			return fileInfo{name: url, size: r.size}, nil
		},
		Create:    func(string) (mkv.WriteSeekCloser, error) { return nil, roErr("create") },
		OpenFile:  func(string, int, os.FileMode) (mkv.ReadWriteSeekCloser, error) { return nil, roErr("open-file") },
		MkdirAll:  func(string, os.FileMode) error { return roErr("mkdir") },
		WriteFile: func(string, []byte, os.FileMode) error { return roErr("write") },
		Remove:    func(string) error { return roErr("remove") },
	}
}

// IsURL reports whether path is an http(s) URL (the scheme httpfs serves).
func IsURL(path string) bool {
	return strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://")
}

// reader is a ReadSeekCloser over one URL: Seek is local, Read serves from the
// current window, fetching a new one on miss.
type reader struct {
	fs   *FS
	url  string
	size int64 // -1 until learned from the first Content-Range
	pos  int64
	buf  []byte
	off  int64 // buf's start offset
}

func (r *reader) Read(p []byte) (int, error) {
	if r.size >= 0 && r.pos >= r.size {
		return 0, io.EOF
	}
	if r.buf == nil || r.pos < r.off || r.pos >= r.off+int64(len(r.buf)) {
		if err := r.fetch(r.pos); err != nil {
			return 0, err
		}
		if r.pos >= r.size {
			return 0, io.EOF
		}
	}
	n := copy(p, r.buf[r.pos-r.off:])
	r.pos += int64(n)
	return n, nil
}

func (r *reader) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.pos + offset
	case io.SeekEnd:
		if r.size < 0 {
			// Learn the size with a minimal ranged request.
			if err := r.fetch(0); err != nil {
				return 0, err
			}
		}
		next = r.size + offset
	default:
		return 0, fmt.Errorf("httpfs: bad whence %d", whence)
	}
	if next < 0 {
		return 0, fmt.Errorf("httpfs: negative seek %d", next)
	}
	r.pos = next
	return next, nil
}

func (r *reader) Close() error { return nil }

// fetch loads the window starting at off via a ranged GET and records the
// total size from the Content-Range header.
func (r *reader) fetch(off int64) error {
	end := off + int64(r.fs.opts.WindowSize) - 1
	if r.size >= 0 {
		if off >= r.size {
			r.buf, r.off = nil, 0
			return nil
		}
		if end >= r.size {
			end = r.size - 1
		}
	}
	req, err := http.NewRequest(http.MethodGet, r.url, nil)
	if err != nil {
		return err
	}
	for k, vs := range r.fs.opts.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))
	resp, err := r.fs.opts.Client.Do(req)
	if err != nil {
		return fmt.Errorf("httpfs: %s: %w", r.url, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		// Content-Range: bytes <start>-<end>/<total>
		cr := resp.Header.Get("Content-Range")
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if total, perr := strconv.ParseInt(cr[i+1:], 10, 64); perr == nil {
				r.size = total
			}
		}
		if r.size < 0 {
			return fmt.Errorf("httpfs: %s: 206 without a usable Content-Range total (%q)", r.url, cr)
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// Reading at/past EOF; keep the known size if any, else this is an error.
		if r.size >= 0 {
			r.buf, r.off = nil, 0
			return nil
		}
		return fmt.Errorf("httpfs: %s: range not satisfiable", r.url)
	case http.StatusOK:
		return fmt.Errorf("httpfs: %s: server ignores Range requests (refusing a full download; the server must support ranged GETs)", r.url)
	default:
		return fmt.Errorf("httpfs: %s: HTTP %s", r.url, resp.Status)
	}
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("httpfs: %s: read body: %w", r.url, err)
	}
	r.fs.fetched.Add(int64(len(buf)))
	r.buf, r.off = buf, off
	return nil
}

// fileInfo is the minimal os.FileInfo Stat returns.
type fileInfo struct {
	name string
	size int64
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() os.FileMode  { return 0o444 }
func (i fileInfo) ModTime() time.Time { return time.Time{} }
func (i fileInfo) IsDir() bool        { return false }
func (i fileInfo) Sys() any           { return nil }

// Hybrid returns an FS that reads http(s) URLs through HTTP Range requests and
// passes every other path — including all writes — to the operating system.
// It is what a "remux from a URL to a local file" invocation needs, since one
// FS serves both the source and the destination:
//
//	mp4.RemuxToMP4(ctx, "https://nas/movie.mkv", "out.mp4", mp4.Options{FS: httpfs.Hybrid()})
//
// A remux reads the source sequentially, so the ranged windows amount to a
// streamed download.
func Hybrid(opts ...Options) *mkv.FS {
	remote := New(opts...).Port()
	return &mkv.FS{
		Open: func(path string) (mkv.ReadSeekCloser, error) {
			if IsURL(path) {
				return remote.DoOpen(path)
			}
			return os.Open(path)
		},
		Stat: func(path string) (os.FileInfo, error) {
			if IsURL(path) {
				return remote.DoStat(path)
			}
			return os.Stat(path)
		},
		// Writes always go to the OS (URLs are read-only anyway).
	}
}
