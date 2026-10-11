// Package httpfs implements the mkv.FS port over HTTP(S) Range requests, so
// any read-only mkvgo operation works on a remote URL as a path. Combined with
// the head-only probe, inspecting a remote file costs a few ranged kilobytes  -
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
	"context"
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
	// RequestTimeout bounds each request, body included; 0 means 60 s, negative none.
	RequestTimeout time.Duration
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
			return f.open(context.Background(), url)
		},
		OpenContext: f.open,
		Stat: func(url string) (os.FileInfo, error) {
			if !IsURL(url) {
				return nil, fmt.Errorf("httpfs: not an http(s) URL: %s", url)
			}
			return f.stat(url)
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

// stat asks the server about the object with one HEAD, or one single-byte
// ranged GET where HEAD is not served; nothing of the body is transferred.
func (f *FS) stat(url string) (os.FileInfo, error) {
	resp, done, err := f.do(context.Background(), http.MethodHead, url, "", "")
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	done()
	if resp.StatusCode == http.StatusOK && resp.ContentLength >= 0 {
		return fileInfo{name: url, size: resp.ContentLength, modTime: lastModified(resp.Header), id: identityOf(resp.Header)}, nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotImplemented {
		return nil, fmt.Errorf("httpfs: %s: HTTP %s", url, resp.Status)
	}
	resp, done, err = f.do(context.Background(), http.MethodGet, url, "bytes=0-0", "")
	if err != nil {
		return nil, err
	}
	defer done()
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		_, _, total, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return nil, fmt.Errorf("httpfs: %s: %w", url, err)
		}
		return fileInfo{name: url, size: total, modTime: lastModified(resp.Header), id: identityOf(resp.Header)}, nil
	case http.StatusOK:
		return nil, errIgnoresRange(url)
	}
	return nil, fmt.Errorf("httpfs: %s: HTTP %s", url, resp.Status)
}

func (f *FS) open(ctx context.Context, url string) (mkv.ReadSeekCloser, error) {
	if !IsURL(url) {
		return nil, fmt.Errorf("httpfs: not an http(s) URL: %s", url)
	}
	return &reader{fs: f, ctx: ctx, url: url, size: -1}, nil
}

// do sends one request bound to ctx and RequestTimeout, with the FS's headers,
// a Range, and the precondition an identity calls for; done releases it once
// the body is read.
func (f *FS) do(ctx context.Context, method, url, rng, identity string) (resp *http.Response, done context.CancelFunc, err error) {
	done = func() {}
	switch t := f.opts.RequestTimeout; {
	case t == 0:
		ctx, done = context.WithTimeout(ctx, defaultRequestTimeout)
	case t > 0:
		ctx, done = context.WithTimeout(ctx, t)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		done()
		return nil, nil, err
	}
	for k, vs := range f.opts.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	switch {
	case strings.HasPrefix(identity, `"`):
		req.Header.Set("If-Match", identity)
	case identity != "" && !strings.HasPrefix(identity, "W/"):
		req.Header.Set("If-Unmodified-Since", identity)
	}
	resp, err = f.opts.Client.Do(req)
	if err != nil {
		done()
		return nil, nil, fmt.Errorf("httpfs: %s: %w", url, err)
	}
	return resp, done, nil
}

const defaultRequestTimeout = 60 * time.Second

func errIgnoresRange(url string) error {
	return fmt.Errorf("httpfs: %s: server ignores Range requests (refusing a full download; the server must support ranged GETs)", url)
}

// parseContentRange reads "bytes <start>-<end>/<total>" and checks start <= end < total.
func parseContentRange(cr string) (start, end, total int64, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(cr), "bytes ")
	if !ok {
		return 0, 0, 0, fmt.Errorf("206 without a usable Content-Range total (%q)", cr)
	}
	span, tot, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, 0, 0, fmt.Errorf("206 without a usable Content-Range total (%q)", cr)
	}
	if total, err = strconv.ParseInt(tot, 10, 64); err != nil || total < 0 {
		return 0, 0, 0, fmt.Errorf("206 without a usable Content-Range total (%q)", cr)
	}
	first, last, _ := strings.Cut(span, "-")
	start, err1 := strconv.ParseInt(first, 10, 64)
	end, err2 := strconv.ParseInt(last, 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start || end >= total {
		return 0, 0, 0, fmt.Errorf("206 with an unusable Content-Range span (%q)", cr)
	}
	return start, end, total, nil
}

// identityOf is the version a response declares: a strong ETag, else Last-Modified, else a weak ETag, else nothing.
func identityOf(h http.Header) string {
	etag, lm := h.Get("ETag"), h.Get("Last-Modified")
	switch {
	case strings.HasPrefix(etag, `"`):
		return etag
	case lm != "":
		return lm
	}
	return etag
}

// strongIdentity reports an identity that is a strong ETag: the only one that changes with every change of the bytes.
func strongIdentity(id string) bool { return strings.HasPrefix(id, `"`) }

// checkVersion verifies a response carries the identity's validator, equal:
// a different one is another version, a missing one proves nothing.
func checkVersion(url, identity string, h http.Header) error {
	if identity == "" {
		return nil
	}
	field := "Last-Modified"
	if strings.HasPrefix(identity, `"`) || strings.HasPrefix(identity, "W/") {
		field = "ETag"
	}
	switch got := h.Get(field); got {
	case identity:
		return nil
	case "":
		return fmt.Errorf("httpfs: %s: the response carries no %s: nothing shows it is the version read so far", url, field)
	}
	return fmt.Errorf("httpfs: %s: %w", url, mkv.ErrSourceChanged)
}

func lastModified(h http.Header) time.Time {
	t, err := http.ParseTime(h.Get("Last-Modified"))
	if err != nil {
		return time.Time{}
	}
	return t
}

// reader is a ReadSeekCloser over one URL: Seek is local, Read serves from the
// current window, fetching a new one on miss. Every window after the first is
// asked for the version the first one declared (If-Match on a strong ETag,
// If-Unmodified-Since on Last-Modified), or the version Pin set, and must
// carry that validator. Only a strong ETag rules out two versions mixed; a
// Last-Modified date does not tell apart two versions written within its second.
type reader struct {
	fs   *FS
	ctx  context.Context // ends the requests in flight (OpenContext)
	url  string
	size int64 // -1 until learned from the first Content-Range
	pos  int64
	buf  []byte
	off  int64  // buf's start offset
	id   string // the version every window must be of: pinned, else the first window's
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

// Pin binds the reader to one version of the object (mkv.PinnedReader): a window of another ends the read with mkv.ErrSourceChanged.
func (r *reader) Pin(identity string) { r.id = identity }

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
	resp, done, err := r.fs.do(r.ctx, http.MethodGet, r.url, fmt.Sprintf("bytes=%d-%d", off, end), r.id)
	if err != nil {
		return err
	}
	defer done()
	defer resp.Body.Close()
	var want int64
	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, last, total, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return fmt.Errorf("httpfs: %s: %w", r.url, err)
		}
		if start != off || last > end {
			return fmt.Errorf("httpfs: %s: 206 served bytes %d-%d for a request of %d-%d", r.url, start, last, off, end)
		}
		if err := checkVersion(r.url, r.id, resp.Header); err != nil {
			return err
		}
		if r.size >= 0 && total != r.size {
			return fmt.Errorf("httpfs: %s: the size went from %d to %d: %w", r.url, r.size, total, mkv.ErrSourceChanged)
		}
		want = last - start + 1
		if resp.ContentLength >= 0 && resp.ContentLength != want {
			return fmt.Errorf("httpfs: %s: 206 of %d bytes declares a body of %d", r.url, want, resp.ContentLength)
		}
		if r.id == "" {
			r.id = identityOf(resp.Header)
		}
		r.size = total
	case http.StatusPreconditionFailed:
		return fmt.Errorf("httpfs: %s: %w", r.url, mkv.ErrSourceChanged)
	case http.StatusRequestedRangeNotSatisfiable:
		// fetch never asks past a known size: a 416 then means the object shrank.
		if r.size >= 0 {
			return fmt.Errorf("httpfs: %s: offset %d is past the object's end now: %w", r.url, off, mkv.ErrSourceChanged)
		}
		return fmt.Errorf("httpfs: %s: range not satisfiable", r.url)
	case http.StatusNotFound, http.StatusGone:
		// Gone under a reader bound to a version is a changed source; a fresh reader's is a plain not-found.
		if r.id != "" || r.size >= 0 {
			return fmt.Errorf("httpfs: %s: HTTP %s: %w", r.url, resp.Status, mkv.ErrSourceChanged)
		}
		return fmt.Errorf("httpfs: %s: HTTP %s", r.url, resp.Status)
	case http.StatusOK:
		return errIgnoresRange(r.url)
	default:
		return fmt.Errorf("httpfs: %s: HTTP %s", r.url, resp.Status)
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, want+1))
	if err != nil {
		return fmt.Errorf("httpfs: %s: read body: %w", r.url, err)
	}
	r.fs.fetched.Add(int64(len(buf)))
	if int64(len(buf)) != want {
		return fmt.Errorf("httpfs: %s: 206 of %d bytes carried %d", r.url, want, len(buf))
	}
	r.buf, r.off = buf, off
	return nil
}

// fileInfo is the os.FileInfo Stat returns: size, Last-Modified, and the version as mkv.FileIdentity.
type fileInfo struct {
	name    string
	size    int64
	modTime time.Time
	id      string
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() os.FileMode  { return 0o444 }
func (i fileInfo) ModTime() time.Time { return i.modTime }
func (i fileInfo) IsDir() bool        { return false }
func (i fileInfo) Sys() any           { return nil }

// Identity is the object's version: its strong ETag, else its Last-Modified, else a weak ETag; what Pin takes.
func (i fileInfo) Identity() string { return i.id }

// Strong reports an identity that is a strong ETag.
func (i fileInfo) Strong() bool { return strongIdentity(i.id) }

// Hybrid returns an FS that reads http(s) URLs through HTTP Range requests and
// passes every other path - including all writes - to the operating system.
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
		OpenContext: func(ctx context.Context, path string) (mkv.ReadSeekCloser, error) {
			if IsURL(path) {
				return remote.DoOpenContext(ctx, path)
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
