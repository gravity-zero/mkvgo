// Package httpfs implements the mkv.FS port over HTTP(S) Range requests, so
// any read-only mkvgo operation works on a remote URL as a path. Combined with
// the head-only probe, inspecting a remote file costs a few ranged kilobytes  -
// indexing a media library on S3/HTTP transfers no full file.
//
//	c, err := matroska.OpenMetaWithFS(ctx, url, httpfs.New().Port())
//
// Reads stream the body of a ranged GET (`Range: bytes=…`), so a parser's many
// small reads don't each become a request and nothing is buffered: a request
// is one window (default 512 KiB), doubles while the reads run on where the
// last one ended, and covers at once a range the caller announced
// (mkv.RangeHinter). The server must honour Range requests (respond 206); one that
// replies 200 to a ranged request gets an explicit error rather than a silent
// full download. The FS is read-only: write operations return an error.
//
// Server-side request forgery: httpfs requests whatever URL it is handed and
// follows redirects, so a service that opens URLs its users supply can be made
// to reach internal hosts (metadata endpoints, admin ports) and read back what
// the parser makes of them. Such a service sets Options.AllowHost to the hosts
// it means to read from; httpfs then refuses any other host, first request and
// redirect alike. Without it, the default client still refuses a redirect to
// another host while Options.Header is set, so credentials put there do not
// follow a redirect out of the host they were meant for.
package httpfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/mkv"
)

const defaultWindow = 512 << 10

// defaultTransport keeps up to 64 idle connections per host, what a server reads at once: http.DefaultTransport
// keeps 2, and past them every read opened a connection (37 % of an origin's requests under 16 clients, measured).
var defaultTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 64
	return t
}()

// Options configures New. The zero value uses a client that follows
// redirects as http.DefaultClient does (see AllowHost for the exception) and
// a 512 KiB read window.
type Options struct {
	// Client is the HTTP client to use; nil means a default client whose
	// redirect policy is AllowHost's. A Client given here keeps its own
	// CheckRedirect: AllowHost then only vets each request's first host.
	Client *http.Client
	// WindowSize is the first request's length in bytes; <= 0 means 512 KiB.
	// A request that continues where the last one ended asks for twice as
	// much, up to 32 windows; a hinted range (mkv.RangeHinter) is asked for
	// whole. Smaller windows transfer less for sparse access.
	WindowSize int
	// Header is added to every request (e.g. Authorization).
	Header http.Header
	// RequestTimeout bounds each request, body included; 0 means 60 s, negative none.
	RequestTimeout time.Duration
	// AllowHost, when set, is asked about the host (host[:port]) of every
	// request and of every redirect the default client meets; a host it
	// refuses is not contacted. Nil allows any host, except that the default
	// client does not follow a redirect to another host while Header is set.
	AllowHost func(host string) bool
}

// FS is the HTTP-backed filesystem. It also counts the bytes fetched, which
// tests and cost-conscious callers can read back.
type FS struct {
	opts    Options
	fetched atomic.Int64
}

// recentReads holds the last responses of a window or less read under a
// strong ETag, for the readers opened under one mkv.ShareReads scope: a reader
// pinned to that version reads them again for no request (a parser's readers
// go over the same head of the file). Outside such a scope nothing is kept:
// every read asks the server, which is how a replaced object is noticed.
type recentReads struct {
	mu     sync.Mutex
	recent []*recentRange
}

// recentRange is the bytes [off, off+len(data)) of one version of a URL. Its
// reader appends to data as the response arrives, within a capacity set up
// front, so a slice taken under FS.mu stays valid.
type recentRange struct {
	url, id string
	off     int64
	size    int64 // the object's total size
	data    []byte
}

// recentMax bounds the responses kept for rereads.
const recentMax = 8

// remember keeps a response, as it is read, for rereads.
func (f *recentReads) remember(e *recentRange) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.recent) == recentMax {
		f.recent = append(f.recent[:0], f.recent[1:]...)
	}
	f.recent = append(f.recent, e)
}

// forget drops a response that failed.
func (f *recentReads) forget(e *recentRange) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, x := range f.recent {
		if x == e {
			f.recent = append(f.recent[:i], f.recent[i+1:]...)
			return
		}
	}
}

// grow appends what e's reader received.
func (f *recentReads) grow(e *recentRange, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if room := cap(e.data) - len(e.data); len(b) > room {
		b = b[:room]
	}
	e.data = append(e.data, b...)
}

// lookup returns the kept bytes of version id of url that hold off.
func (f *recentReads) lookup(url, id string, off int64) (recentRange, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.recent) - 1; i >= 0; i-- {
		e := *f.recent[i]
		if e.url == url && e.id == id && off >= e.off && off < e.off+int64(len(e.data)) {
			return e, true
		}
	}
	return recentRange{}, false
}

// New returns an HTTP Range-backed FS. Paths passed to the returned port must
// be absolute http:// or https:// URLs.
func New(opts ...Options) *FS {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Client == nil {
		o.Client = &http.Client{Transport: defaultTransport, CheckRedirect: redirectPolicy(o)}
	}
	if o.WindowSize <= 0 {
		o.WindowSize = defaultWindow
	}
	return &FS{opts: o}
}

// redirectPolicy is the default client's: at most 10 hops, each to a host
// AllowHost accepts, or with no AllowHost to the first request's host
// whenever Header is set, and never from https to http with Header set, so the
// caller's headers never reach another host or cross in clear.
func redirectPolicy(o Options) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		host := req.URL.Host
		switch {
		case o.AllowHost != nil && !o.AllowHost(host):
			return fmt.Errorf("redirect to host %s refused by AllowHost", host)
		case o.AllowHost == nil && len(o.Header) > 0 && host != via[0].URL.Host:
			return fmt.Errorf("redirect from %s to another host %s refused: the request carries Options.Header (set AllowHost to allow it)", via[0].URL.Host, host)
		case len(o.Header) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https":
			return fmt.Errorf("redirect from https to %s on %s refused: the request carries Options.Header, which would cross in clear", req.URL.Scheme, host)
		}
		return nil
	}
}

// hideQuery strips the query from the URL a *url.Error names.
func hideQuery(err error) error {
	var ue *neturl.Error
	if errors.As(err, &ue) {
		ue.URL = shown.Path(ue.URL)
	}
	return err
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
				return nil, fmt.Errorf("httpfs: not an http(s) URL: %s", shown.Path(url))
			}
			return f.stat(url)
		},
		Create:    func(string) (mkv.WriteSeekCloser, error) { return nil, roErr("create") },
		OpenFile:  func(string, int, os.FileMode) (mkv.ReadWriteSeekCloser, error) { return nil, roErr("open-file") },
		MkdirAll:  func(string, os.FileMode) error { return roErr("mkdir") },
		WriteFile: func(string, []byte, os.FileMode) error { return roErr("write") },
		Remove:    func(string) error { return roErr("remove") },
		Rename:    func(string, string) error { return roErr("rename") },
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
		return fileInfo{name: shown.Path(url), size: resp.ContentLength, modTime: lastModified(resp.Header), id: identityOf(resp.Header)}, nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotImplemented {
		return nil, fmt.Errorf("httpfs: %s: HTTP %s", shown.Path(url), resp.Status)
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
			return nil, fmt.Errorf("httpfs: %s: %w", shown.Path(url), err)
		}
		return fileInfo{name: shown.Path(url), size: total, modTime: lastModified(resp.Header), id: identityOf(resp.Header)}, nil
	case http.StatusOK:
		return nil, errIgnoresRange(url)
	}
	return nil, fmt.Errorf("httpfs: %s: HTTP %s", shown.Path(url), resp.Status)
}

func (f *FS) open(ctx context.Context, url string) (mkv.ReadSeekCloser, error) {
	if !IsURL(url) {
		return nil, fmt.Errorf("httpfs: not an http(s) URL: %s", shown.Path(url))
	}
	r := &reader{fs: f, ctx: ctx, url: url, size: -1}
	if share := mkv.ReadShareFrom(ctx); share != nil {
		r.share = share.LoadOrStore(f, &recentReads{}).(*recentReads)
	}
	return r, nil
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
		return nil, nil, hideQuery(err)
	}
	if f.opts.AllowHost != nil && !f.opts.AllowHost(req.URL.Host) {
		done()
		return nil, nil, fmt.Errorf("httpfs: %s: host %s refused by AllowHost", shown.Path(url), req.URL.Host)
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
		return nil, nil, fmt.Errorf("httpfs: %s: %w", shown.Path(url), hideQuery(err))
	}
	return resp, done, nil
}

const defaultRequestTimeout = 60 * time.Second

func errIgnoresRange(url string) error {
	return fmt.Errorf("httpfs: %s: server ignores Range requests (refusing a full download; the server must support ranged GETs)", shown.Path(url))
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
		return fmt.Errorf("httpfs: %s: the response carries no %s: nothing shows it is the version read so far", shown.Path(url), field)
	}
	return fmt.Errorf("httpfs: %s: %w", shown.Path(url), mkv.ErrSourceChanged)
}

func lastModified(h http.Header) time.Time {
	t, err := http.ParseTime(h.Get("Last-Modified"))
	if err != nil {
		return time.Time{}
	}
	return t
}

// reader is a ReadSeekCloser over one URL. Seek is local; Read streams the
// body of a ranged GET, so the bytes are consumed as they arrive and never
// held. A read that is not where the response stands - backwards, past its
// end, or far ahead in it - starts another request. A request is one window long,
// doubles on each request that continues where the last one ended (up to 32
// windows), and covers a range set by HintRange in one go. A read up to a
// mebibyte ahead within the response reads through to it. Every response
// after the first is asked for the version the first one declared (If-Match
// on a strong ETag, If-Unmodified-Since on Last-Modified), or the version Pin
// set, and must carry that validator. Only a strong ETag rules out two
// versions mixed; a Last-Modified date does not tell apart two versions
// written within its second.
type reader struct {
	fs   *FS
	ctx  context.Context // ends the requests in flight (OpenContext)
	url  string
	size int64 // -1 until learned from the first Content-Range
	pos  int64
	id   string // the version every response must be of: pinned, else the first response's

	body     io.ReadCloser      // the response being read, nil when none
	done     context.CancelFunc // releases body's request
	bodyPos  int64              // source offset of body's next byte
	bodyEnd  int64              // source offset past body's last byte
	span     int64              // length asked by the last request
	breaks   int                // responses that broke midway and were resumed
	hintOff  int64
	hintEnd  int64        // [hintOff, hintEnd): the range the next reads cover (HintRange); empty when equal
	lastStop int64        // where the last response ended: a request starting there continues a sequential run
	share    *recentReads // the responses shared under the mkv.ShareReads scope the reader was opened in, nil outside one
	kept     bool         // body serves bytes kept from an earlier response, not the network
	keep     *recentRange // body's bytes, kept for rereads as they arrive (a response of a window or less)
}

// skipMax is the most of a response read through to reach a byte further in
// it, rather than ask for that byte with a new request.
const skipMax = 1 << 20

// maxBreaks bounds the responses of one reader resumed after breaking midway.
const maxBreaks = 4

// drainMax is the most of an abandoned response read out to keep its
// connection for the next request: closing it unread drops the connection,
// and the next request pays a new handshake (TCP, and TLS) for bytes that
// were mostly on their way already. A larger rest is dropped with it.
const drainMax = 1 << 20

func (r *reader) Read(p []byte) (int, error) {
	if r.size >= 0 && r.pos >= r.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	// Ahead of the response but within it: its bytes are on their way, read
	// through the gap rather than pay a round trip (up to skipMax or a window).
	if r.body != nil && r.pos > r.bodyPos && r.pos < r.bodyEnd && r.pos-r.bodyPos <= max(skipMax, int64(r.fs.opts.WindowSize)) {
		if err := r.skip(r.pos - r.bodyPos); err != nil {
			return 0, err
		}
	}
	if r.body == nil || r.pos != r.bodyPos || r.pos >= r.bodyEnd {
		if err := r.fetch(r.pos); err != nil {
			return 0, err
		}
		if r.body == nil { // at or past the end
			return 0, io.EOF
		}
	}
	if rest := r.bodyEnd - r.bodyPos; int64(len(p)) > rest {
		p = p[:rest]
	}
	n, err := r.body.Read(p)
	r.bodyPos += int64(n)
	r.pos += int64(n)
	r.account(p[:n])
	switch {
	case r.bodyPos == r.bodyEnd:
		// The whole range came: the body must end here.
		if err == nil {
			var one [1]byte
			m, e := io.ReadFull(r.body, one[:])
			r.fs.fetched.Add(int64(m))
			if m > 0 || (e != io.EOF && e != io.ErrUnexpectedEOF) {
				// The last bytes are withheld: nothing shows the response is what was asked.
				r.abandon()
				r.pos -= int64(n)
				if m > 0 {
					return 0, fmt.Errorf("httpfs: %s: 206 of %d bytes carried more", shown.Path(r.url), r.span)
				}
				return 0, fmt.Errorf("httpfs: %s: read body: %w", shown.Path(r.url), e)
			}
		}
		r.release()
		return n, nil
	case err == io.EOF:
		r.abandon()
		return n, fmt.Errorf("httpfs: %s: 206 ended %d bytes short of its range", shown.Path(r.url), r.bodyEnd-r.bodyPos)
	case err != nil:
		// A response that broke midway (a timeout over a slow reader, a
		// dropped connection) is resumed from where it stopped, a few times
		// in the reader's life.
		// The resumption is asked for the version read so far (If-Match): with
		// no version known, nothing would tell two versions apart, so none.
		r.abandon()
		if r.ctx.Err() != nil || r.breaks >= maxBreaks || r.id == "" {
			return n, fmt.Errorf("httpfs: %s: read body: %w", shown.Path(r.url), err)
		}
		r.breaks++
		if n > 0 {
			return n, nil
		}
		return r.Read(p)
	}
	return n, nil
}

// account counts bytes taken off the network, and keeps them for rereads when the response is small.
func (r *reader) account(b []byte) {
	if r.kept {
		return
	}
	r.fs.fetched.Add(int64(len(b)))
	if r.keep != nil {
		r.share.grow(r.keep, b)
	}
}

// sink is where bytes read through go: accounted and kept like the bytes read.
func (r *reader) sink() io.Writer { return sinkFunc(func(b []byte) { r.account(b) }) }

type sinkFunc func([]byte)

func (f sinkFunc) Write(b []byte) (int, error) { f(b); return len(b), nil }

// skip reads n bytes of the response through, unread.
func (r *reader) skip(n int64) error {
	m, err := io.CopyN(r.sink(), r.body, n)
	r.bodyPos += m
	if err != nil {
		r.abandon()
		if err == io.EOF {
			return fmt.Errorf("httpfs: %s: 206 ended %d bytes short of its range", shown.Path(r.url), r.bodyEnd-r.bodyPos)
		}
		if r.ctx.Err() != nil {
			return fmt.Errorf("httpfs: %s: read body: %w", shown.Path(r.url), err)
		}
	}
	return nil
}

// abandon releases a response that failed: none of it is kept for rereads.
func (r *reader) abandon() {
	if r.keep != nil {
		r.share.forget(r.keep)
		r.keep = nil
	}
	r.release()
}

// release ends the response being read: a small rest is read out so the
// connection serves the next request, a large one is dropped with it.
func (r *reader) release() {
	if r.body == nil {
		return
	}
	// A response kept for rereads is read out whole: the next reader of these bytes takes them from it.
	if rest := r.bodyEnd - r.bodyPos; rest <= drainMax || r.keep != nil {
		m, _ := io.CopyN(r.sink(), r.body, rest+1)
		r.bodyPos += m
	}
	r.body.Close()
	r.done()
	r.keep, r.kept = nil, false
	r.bodyPos = min(r.bodyPos, r.bodyEnd)
	r.lastStop = r.bodyPos
	r.body, r.done = nil, nil
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

func (r *reader) Close() error {
	r.release()
	return nil
}

// Pin binds the reader to one version of the object (mkv.PinnedReader): a response of another ends the read with mkv.ErrSourceChanged.
func (r *reader) Pin(identity string) { r.id = identity }

// Free reports whether n bytes at off come with the response being read, with no new request.
func (r *reader) Free(off, n int64) bool {
	return r.body != nil && off >= r.bodyPos && off+n <= r.bodyEnd && off-r.bodyPos <= max(skipMax, int64(r.fs.opts.WindowSize))
}

// HintRange tells the reader its next reads cover [off, end) (mkv.RangeHinter): the request that reaches into it asks for the rest of it at once.
func (r *reader) HintRange(off, end int64) { r.hintOff, r.hintEnd = off, end }

// ReadAt reads len(p) bytes at off with one ranged GET of exactly that span
// (io.ReaderAt), of the version the reader is pinned to; it leaves the
// reader's position and response alone, so several may run at once.
func (r *reader) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.share != nil && strongIdentity(r.id) {
		if e, ok := r.share.lookup(r.url, r.id, off); ok && off+int64(len(p)) <= e.off+int64(len(e.data)) {
			return copy(p, e.data[off-e.off:]), nil
		}
	}
	resp, done, err := r.fs.do(r.ctx, http.MethodGet, r.url, fmt.Sprintf("bytes=%d-%d", off, off+int64(len(p))-1), r.id)
	if err != nil {
		return 0, err
	}
	defer done()
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusPreconditionFailed:
		return 0, fmt.Errorf("httpfs: %s: %w", shown.Path(r.url), mkv.ErrSourceChanged)
	case http.StatusRequestedRangeNotSatisfiable:
		return 0, io.EOF
	case http.StatusOK:
		return 0, errIgnoresRange(r.url)
	default:
		return 0, fmt.Errorf("httpfs: %s: HTTP %s", shown.Path(r.url), resp.Status)
	}
	start, last, total, err := parseContentRange(resp.Header.Get("Content-Range"))
	if err != nil {
		return 0, fmt.Errorf("httpfs: %s: %w", shown.Path(r.url), err)
	}
	if start != off || last >= off+int64(len(p)) {
		return 0, fmt.Errorf("httpfs: %s: 206 served bytes %d-%d for a request of %d-%d", shown.Path(r.url), start, last, off, off+int64(len(p))-1)
	}
	if err := checkVersion(r.url, r.id, resp.Header); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(resp.Body, p[:last-start+1])
	r.fs.fetched.Add(int64(n))
	if err != nil {
		return n, fmt.Errorf("httpfs: %s: read body: %w", shown.Path(r.url), err)
	}
	if n < len(p) {
		if total >= 0 && last+1 >= total {
			return n, io.EOF
		}
		// The server served less than asked short of the object's end (a range cap): the rest is asked for again.
		m, err := r.ReadAt(p[n:], off+int64(n))
		return n + m, err
	}
	return n, nil
}

// Stat answers for the object the reader reads. Opened under a mkv.ShareReads
// scope, it asks for the first and the last window at once instead of a HEAD
// - a parser reads both ends next - and keeps them for the scope's readers:
// the size and version come from the first response. Outside a scope it is
// FS.Stat.
func (r *reader) Stat() (os.FileInfo, error) {
	if r.share == nil {
		return r.fs.stat(r.url)
	}
	w := int64(r.fs.opts.WindowSize)
	var tail *keptRange
	var tailErr error
	tailDone := make(chan struct{})
	go func() {
		defer close(tailDone)
		tail, tailErr = r.fetchKept(w, true)
	}()
	head, err := r.fetchKept(w, false)
	<-tailDone
	if err != nil {
		if errors.Is(err, errNotRanged) {
			return r.fs.stat(r.url)
		}
		return nil, err
	}
	if tailErr == nil && (tail.id != head.id || tail.size != head.size) { // the two ends are of two versions
		return nil, fmt.Errorf("httpfs: %s: %w", shown.Path(r.url), mkv.ErrSourceChanged)
	}
	if !strongIdentity(head.id) {
		return fileInfo{name: shown.Path(r.url), size: head.size, modTime: head.mod, id: head.id}, nil
	}
	r.share.remember(&head.recentRange)
	if tailErr == nil {
		r.share.remember(&tail.recentRange)
	}
	return fileInfo{name: shown.Path(r.url), size: head.size, modTime: head.mod, id: head.id}, nil
}

// errNotRanged is a 416 to a first request: an empty object, whose stat a HEAD answers.
var errNotRanged = errors.New("httpfs: range not satisfiable")

// keptRange is a whole response read for a ReadShare scope, with the Last-Modified it declared.
type keptRange struct {
	recentRange
	mod time.Time
}

// fetchKept reads the whole response to a ranged GET of the first w bytes, or
// the last w when suffix is set; a response serving anything else is refused
// before its body is read, so nothing past w is ever allocated.
func (r *reader) fetchKept(w int64, suffix bool) (*keptRange, error) {
	rng := fmt.Sprintf("bytes=0-%d", w-1)
	if suffix {
		rng = fmt.Sprintf("bytes=-%d", w)
	}
	resp, done, err := r.fs.do(r.ctx, http.MethodGet, r.url, rng, "")
	if err != nil {
		return nil, err
	}
	defer done()
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusRequestedRangeNotSatisfiable:
		return nil, errNotRanged
	case http.StatusOK:
		return nil, errIgnoresRange(r.url)
	default:
		return nil, fmt.Errorf("httpfs: %s: HTTP %s", shown.Path(r.url), resp.Status)
	}
	start, last, total, err := parseContentRange(resp.Header.Get("Content-Range"))
	if err != nil {
		return nil, fmt.Errorf("httpfs: %s: %w", shown.Path(r.url), err)
	}
	want := last - start + 1
	if (!suffix && (start != 0 || last > w-1)) || (suffix && (last != total-1 || want > w)) {
		return nil, fmt.Errorf("httpfs: %s: 206 served bytes %d-%d for a request of %s", shown.Path(r.url), start, last, rng)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != want {
		return nil, fmt.Errorf("httpfs: %s: 206 of %d bytes declares a body of %d", shown.Path(r.url), want, resp.ContentLength)
	}
	data := make([]byte, want)
	n, err := io.ReadFull(resp.Body, data)
	r.fs.fetched.Add(int64(n))
	if err != nil {
		return nil, fmt.Errorf("httpfs: %s: read body: %w", shown.Path(r.url), err)
	}
	return &keptRange{recentRange: recentRange{url: r.url, id: identityOf(resp.Header), off: start, size: total, data: data}, mod: lastModified(resp.Header)}, nil
}

// fetch starts the ranged GET of the response that serves off, and records
// the total size from its Content-Range header.
func (r *reader) fetch(off int64) error {
	r.release()
	window := int64(r.fs.opts.WindowSize)
	span := window
	hinted := off >= r.hintOff && off < r.hintEnd
	switch {
	case hinted:
		span = max(r.hintEnd-off, window)
	case off == r.lastStop && r.span > 0:
		span = min(2*r.span, 32*window)
	}
	end := off + span - 1
	if r.size >= 0 {
		if off >= r.size {
			return nil
		}
		if end >= r.size {
			end = r.size - 1
		}
	}
	if r.share != nil && strongIdentity(r.id) {
		if e, ok := r.share.lookup(r.url, r.id, off); ok && (r.size < 0 || r.size == e.size) {
			r.size = e.size
			r.body, r.done = io.NopCloser(bytes.NewReader(e.data[off-e.off:])), func() {}
			r.bodyPos, r.bodyEnd, r.kept = off, e.off+int64(len(e.data)), true
			return nil
		}
	}
	rng := fmt.Sprintf("bytes=%d-%d", off, end)
	if end >= math.MaxInt64/2 { // a hint to the end of an object of unknown size
		rng = fmt.Sprintf("bytes=%d-", off)
	}
	resp, done, err := r.fs.do(r.ctx, http.MethodGet, r.url, rng, r.id)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			resp.Body.Close()
			done()
		}
	}()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, last, total, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return fmt.Errorf("httpfs: %s: %w", shown.Path(r.url), err)
		}
		if start != off || last > end {
			return fmt.Errorf("httpfs: %s: 206 served bytes %d-%d for a request of %d-%d", shown.Path(r.url), start, last, off, end)
		}
		if err := checkVersion(r.url, r.id, resp.Header); err != nil {
			return err
		}
		if r.size >= 0 && total != r.size {
			return fmt.Errorf("httpfs: %s: the size went from %d to %d: %w", shown.Path(r.url), r.size, total, mkv.ErrSourceChanged)
		}
		want := last - start + 1
		if resp.ContentLength >= 0 && resp.ContentLength != want {
			return fmt.Errorf("httpfs: %s: 206 of %d bytes declares a body of %d", shown.Path(r.url), want, resp.ContentLength)
		}
		if r.id == "" {
			r.id = identityOf(resp.Header)
		}
		r.size = total
		r.span = want
		if hinted { // past a hinted range, a run starts over from one window
			r.span = window
		}
		r.body, r.done, r.bodyPos, r.bodyEnd = resp.Body, done, off, last+1
		if r.share != nil && want <= window && strongIdentity(r.id) {
			r.keep = &recentRange{url: r.url, id: r.id, off: off, size: total, data: make([]byte, 0, want)}
			r.share.remember(r.keep)
		}
		ok = true
		return nil
	case http.StatusPreconditionFailed:
		return fmt.Errorf("httpfs: %s: %w", shown.Path(r.url), mkv.ErrSourceChanged)
	case http.StatusRequestedRangeNotSatisfiable:
		// fetch never asks past a known size: a 416 then means the object shrank.
		if r.size >= 0 {
			return fmt.Errorf("httpfs: %s: offset %d is past the object's end now: %w", shown.Path(r.url), off, mkv.ErrSourceChanged)
		}
		return fmt.Errorf("httpfs: %s: range not satisfiable", shown.Path(r.url))
	case http.StatusNotFound, http.StatusGone:
		// Gone under a reader bound to a version is a changed source; a fresh reader's is a plain not-found.
		if r.id != "" || r.size >= 0 {
			return fmt.Errorf("httpfs: %s: HTTP %s: %w", shown.Path(r.url), resp.Status, mkv.ErrSourceChanged)
		}
		return fmt.Errorf("httpfs: %s: HTTP %s", shown.Path(r.url), resp.Status)
	case http.StatusOK:
		return errIgnoresRange(r.url)
	}
	return fmt.Errorf("httpfs: %s: HTTP %s", shown.Path(r.url), resp.Status)
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
