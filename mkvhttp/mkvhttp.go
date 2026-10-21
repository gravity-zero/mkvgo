// Package mkvhttp is a drop-in http.Handler for the on-demand plans this
// repository already builds head-only (mp4.HLSPlan, mp4.ABRPlan): nothing is
// pre-generated on disk, every resource a player requests is built from the
// source the first time it is asked for, and static-VOD HTTP semantics
// (ETag, conditional GET, Range, Cache-Control) come for free. Every resource
// is served "no-cache" by default, revalidated on its ETag: a segment name
// does not say which version of the source it was built from. Options.Immutable
// opts the init and media segments into a year of immutable caching, for a
// handler mounted where the URL names the source's version (plan.SourceID()).
//
//	plan, _ := mp4.PlanHLS(ctx, "movie.mkv", mp4.Options{})
//	http.Handle("/hls/", http.StripPrefix("/hls/", mkvhttp.Handler(plan)))
//
// mp4.HLSPlan and mp4.ABRPlan already satisfy Resolver as-is - both declare
// `func (p *T) Resource(ctx context.Context, name string) ([]byte, string, error)` -
// so Handler(plan) works directly with either; no adapter is needed. A
// Resolver backed by anything else can be written by hand, or wrapped with
// ResolverFunc for a plain function.
package mkvhttp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gravity-zero/mkvgo/mp4"
)

// Resolver builds one named resource on demand.
type Resolver interface {
	Resource(ctx context.Context, name string) (data []byte, contentType string, err error)
}

// ResolverFunc adapts a plain function to Resolver, mirroring http.HandlerFunc.
type ResolverFunc func(ctx context.Context, name string) ([]byte, string, error)

// Resource calls f.
func (f ResolverFunc) Resource(ctx context.Context, name string) ([]byte, string, error) {
	return f(ctx, name)
}

// ErrNotFound is the sentinel a Resolver returns - or wraps, via
// fmt.Errorf("...: %w", mkvhttp.ErrNotFound) - to have Handler answer 404
// instead of the default 502 for a resource name it does not recognise.
var ErrNotFound = errors.New("mkvhttp: resource not found")

// Options configures Handler. The zero value is a plain same-origin handler.
type Options struct {
	// AllowCORS adds the permissive CORS headers a browser-based player needs
	// to fetch resources across origins: Access-Control-Allow-Origin: *,
	// exposed headers for Range/ETag, and a 204 response to an OPTIONS
	// preflight request.
	AllowCORS bool
	// Immutable announces the init and media segments as cacheable for a
	// year without revalidation. Set it only when the URL names the source's
	// version (mounted under its plan.SourceID(), say); without it they are
	// served "no-cache": a client revalidates them on their ETag.
	Immutable bool
	// Buffered serves every resource from its bytes even when the Resolver
	// can stream (Opener): the behaviour before streaming existed, a strong
	// ETag included.
	Buffered bool
	// MaxInFlight caps the resources served at once, playlists and manifests
	// aside; 0 is no cap. Each holds a buffer, or a whole segment, until its
	// client has read it: past what the source can feed, requests pile up and
	// memory with them. Over the cap a request waits for a slot.
	MaxInFlight int
	// MaxWaiting bounds the requests waiting under MaxInFlight; one more is
	// answered 503 with Retry-After: 1 and Cache-Control: no-store.
	MaxWaiting int
}

// Opener is what a plan that writes a resource on demand offers
// (mp4.HLSPlan.Open): Handler then streams a media segment from the source
// through one buffer instead of holding the segment while the client reads it.
type Opener interface {
	Open(ctx context.Context, name string) (*mp4.ResourceHandle, error)
}

// Handler serves r's resources over HTTP with static-VOD semantics:
//
//   - GET and HEAD only (405 otherwise, Allow header set); OPTIONS gets a
//     204 CORS preflight response when Options.AllowCORS is set.
//   - Resource name = the request path with its leading slash trimmed; mount
//     under a prefix with http.StripPrefix, the same way any other handler
//     serving a sub-path would.
//   - Strong ETag: the SHA-256 of the resource's bytes, quoted. An
//     If-None-Match that matches gets a bare 304.
//   - Content-Type comes from the Resolver, never sniffed from the name -
//     it is set on the response BEFORE handing off to http.ServeContent, so
//     ServeContent's own name-extension detection never overrides it.
//   - Range requests are served by http.ServeContent (over a bytes.Reader, no
//     modtime - the ETag already identifies the exact bytes).
//   - A Resolver that is also an Opener (mp4.HLSPlan) has its media segments
//     streamed from the source through one buffer, never held whole: exact
//     Content-Length, a weak ETag, single Range (ignored under an If-Range,
//     which a weak ETag never satisfies), and a connection cut short
//     when the source fails midway. Options.Buffered keeps the old path.
//   - Cache-Control: "no-cache" by default - a client keeps a resource and
//     revalidates it on its ETag (a bare 304 when unchanged). A segment name
//     maps to the same bytes for a given source, but not across a source
//     replaced under the same path, and the name does not say which source it
//     is: a year of immutable caching there would serve the old file's
//     segments against the new playlist. Options.Immutable announces the
//     init and media segments "public, max-age=31536000, immutable" for a
//     handler mounted where the URL names the source's version (under its
//     plan.SourceID()); playlists and manifests stay "no-cache".
//   - Options.MaxInFlight bounds the resources served at once and
//     Options.MaxWaiting the requests queued behind them; a request past both
//     is answered 503 with Retry-After: 1 and Cache-Control: no-store, so no
//     cache in front keeps the refusal.
//   - A Resolver error that is (or wraps) ErrNotFound or mp4.ErrNoResource answers 404; any other
//     error answers 502 with a terse body (the source read failed, which is
//     not the client's fault).
func Handler(r Resolver, opts ...Options) http.Handler {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	return &handler{resolver: r, opts: o, gate: newGate(o)}
}

type handler struct {
	resolver Resolver
	opts     Options
	gate     *gate // nil without Options.MaxInFlight
}

func (h *handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if h.opts.AllowCORS {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, ETag, Accept-Ranges")
	}

	if req.Method == http.MethodOptions {
		if !h.opts.AllowCORS {
			methodNotAllowed(w)
			return
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Range, If-None-Match")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}

	name := strings.TrimPrefix(req.URL.Path, "/")
	if name == "" {
		http.Error(w, "mkvhttp: empty resource name", http.StatusNotFound)
		return
	}
	if h.gate != nil && gated(name) {
		release, ok := h.gate.enter(req.Context())
		if !ok {
			busy(w)
			return
		}
		defer release()
	}

	var data []byte
	var contentType string
	var err error
	if op, ok := h.resolver.(Opener); ok && !h.opts.Buffered {
		if st, ok := h.resolver.(stater); ok && (req.Method == http.MethodHead || req.Header.Get("If-None-Match") != "") {
			size, etag, ct, known, err := st.Stat(req.Context(), name)
			if err == nil && known && h.answerWithoutBody(w, req, name, size, etag, ct) {
				return
			}
		}
		var rh *mp4.ResourceHandle
		rh, err = op.Open(req.Context(), name)
		if err == nil && !rh.Whole() {
			h.serveStreamed(w, req, name, rh)
			return
		}
		if err == nil {
			h.serveWhole(w, req, name, rh)
			return
		}
	} else {
		data, contentType, err = h.resolver.Resource(req.Context(), name)
	}
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, mp4.ErrNoResource) {
			http.Error(w, "mkvhttp: resource not found", http.StatusNotFound)
			return
		}
		http.Error(w, "mkvhttp: resolver error", http.StatusBadGateway)
		return
	}

	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", cacheControlFor(name, h.opts.Immutable))
	if inm := req.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}

	http.ServeContent(w, req, name, time.Time{}, bytes.NewReader(data))
}

// serveStreamed answers from a streaming handle: exact Content-Length, a weak
// ETag (the bytes are never in hand), one Range at a time, and on an error
// after the status line a connection cut short - a player must never take a
// truncated segment for a whole one.
func (h *handler) serveStreamed(w http.ResponseWriter, req *http.Request, name string, rh *mp4.ResourceHandle) {
	defer rh.Close()
	start, n, done := h.streamedHead(w, req, name, rh.Size(), rh.ETag(), rh.ContentType())
	if done {
		return
	}
	if _, err := rh.WriteRange(req.Context(), w, start, n); err != nil {
		panic(http.ErrAbortHandler)
	}
}

// serveWhole answers from a handle holding the resource whole, as from bytes in hand (strong ETag, any Range), its
// bytes read through the handle so the plan counts what is sent: nothing for a HEAD or a 304, a range its part.
func (h *handler) serveWhole(w http.ResponseWriter, req *http.Request, name string, rh *mp4.ResourceHandle) {
	defer rh.Close()
	etag := rh.ETag()
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", cacheControlFor(name, h.opts.Immutable))
	if inm := req.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if ct := rh.ContentType(); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, req, name, time.Time{}, io.NewSectionReader(handleReaderAt{req.Context(), rh}, 0, rh.Size()))
}

// handleReaderAt reads a whole handle's bytes through WriteRange, which counts them.
type handleReaderAt struct {
	ctx context.Context
	rh  *mp4.ResourceHandle
}

func (r handleReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n := min(int64(len(p)), r.rh.Size()-off)
	if n <= 0 {
		return 0, io.EOF
	}
	b := bytes.NewBuffer(p[:0])
	m, err := r.rh.WriteRange(r.ctx, b, off, n)
	if err == nil && n < int64(len(p)) {
		err = io.EOF
	}
	return int(m), err
}

// stater is what a plan that can describe a streamed resource without serving it offers (mp4.HLSPlan.Stat).
type stater interface {
	Stat(ctx context.Context, name string) (size int64, etag, contentType string, ok bool, err error)
}

// answerWithoutBody answers a HEAD or a matching revalidation from what Stat says, the segment never opened; false
// when the request needs the body.
func (h *handler) answerWithoutBody(w http.ResponseWriter, req *http.Request, name string, size int64, etag, contentType string) bool {
	if req.Method != http.MethodHead && !etagMatches(req.Header.Get("If-None-Match"), etag) {
		return false
	}
	h.streamedHead(w, req, name, size, etag, contentType)
	return true
}

// streamedHead writes a streamed resource's status line and headers: done when nothing follows (a 304, a 416, a
// HEAD), else the span to write.
func (h *handler) streamedHead(w http.ResponseWriter, req *http.Request, name string, size int64, etag, contentType string) (start, n int64, done bool) {
	w.Header().Set("ETag", etag)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", cacheControlFor(name, h.opts.Immutable))
	if inm := req.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return 0, 0, true
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	rangeHeader := req.Header.Get("Range")
	if req.Header.Get("If-Range") != "" {
		rangeHeader = "" // a weak ETag never matches If-Range's strong comparison and there is no Last-Modified: the whole resource (RFC 9110 13.1.5)
	}
	start, n, status, ok := byteRange(rangeHeader, size)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "mkvhttp: range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return 0, 0, true
	}
	if status == http.StatusPartialContent {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+n-1, size))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	w.WriteHeader(status)
	return start, n, req.Method == http.MethodHead
}

// byteRange parses a single "bytes=a-b" Range header against size: the span to
// write and the status (200 when there is no Range, 206 otherwise); ok is false
// when the range cannot be satisfied. A multi-range request is served whole.
func byteRange(header string, size int64) (start, n int64, status int, ok bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, size, http.StatusOK, true
	}
	if size == 0 {
		return 0, 0, 0, false // no byte of an empty resource can be ranged
	}
	a, b, _ := strings.Cut(spec, "-")
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	switch {
	case a == "" && b != "": // the last b bytes
		k, err := strconv.ParseInt(b, 10, 64)
		if err != nil || k <= 0 {
			return 0, 0, 0, false
		}
		k = min(k, size)
		return size - k, k, http.StatusPartialContent, true
	case a != "":
		s, err := strconv.ParseInt(a, 10, 64)
		if err != nil || s < 0 || s >= size {
			return 0, 0, 0, false
		}
		e := size - 1
		if b != "" {
			if e, err = strconv.ParseInt(b, 10, 64); err != nil || e < s {
				return 0, 0, 0, false
			}
			e = min(e, size-1)
		}
		return s, e - s + 1, http.StatusPartialContent, true
	}
	return 0, 0, 0, false
}

// methodNotAllowed answers a non-GET/HEAD/OPTIONS request; shared by Handler
// and FileHandler, whose method-not-allowed responses are identical.
func methodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "mkvhttp: method not allowed", http.StatusMethodNotAllowed)
}

// etagMatches reports whether etag matches a member of the If-None-Match list
// under the weak comparison (RFC 9110 8.8.3.2, W/ prefixes do not count), or that list is "*".
func etagMatches(header, etag string) bool {
	if strings.TrimSpace(header) == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, part := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(part), "W/") == want {
			return true
		}
	}
	return false
}

// cacheControlFor returns the Cache-Control value for a resource name; see
// the Handler doc for the reasoning.
func cacheControlFor(name string, immutable bool) string {
	if !immutable || strings.HasSuffix(name, ".m3u8") || strings.HasSuffix(name, ".mpd") {
		return "no-cache"
	}
	return immutableCache
}

const immutableCache = "public, max-age=31536000, immutable"
