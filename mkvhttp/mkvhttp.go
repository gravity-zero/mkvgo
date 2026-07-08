// Package mkvhttp is a drop-in http.Handler for the on-demand plans this
// repository already builds head-only (mp4.HLSPlan, mp4.ABRPlan): nothing is
// pre-generated on disk, every resource a player requests is built from the
// source the first time it is asked for, and static-VOD HTTP semantics
// (strong ETag, conditional GET, Range, long-lived caching for the
// deterministic outputs) come for free.
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
	"net/http"
	"strings"
	"time"
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
//   - Cache-Control: a playlist/manifest (.m3u8/.mpd) gets "no-cache" (its
//     bytes name segments that can be re-derived as the plan evolves);
//     every other resource gets "public, max-age=31536000, immutable" - safe
//     because a segment/init name always maps to the exact same bytes for a
//     given source (PlanHLS/PlanABR's determinism guarantee).
//   - A Resolver error that is (or wraps) ErrNotFound answers 404; any other
//     error answers 502 with a terse body (the source read failed, which is
//     not the client's fault).
func Handler(r Resolver, opts ...Options) http.Handler {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	return &handler{resolver: r, opts: o}
}

type handler struct {
	resolver Resolver
	opts     Options
}

func (h *handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if h.opts.AllowCORS {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, ETag, Accept-Ranges")
	}

	if req.Method == http.MethodOptions {
		if !h.opts.AllowCORS {
			h.methodNotAllowed(w)
			return
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Range, If-None-Match")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		h.methodNotAllowed(w)
		return
	}

	name := strings.TrimPrefix(req.URL.Path, "/")
	if name == "" {
		http.Error(w, "mkvhttp: empty resource name", http.StatusNotFound)
		return
	}

	data, contentType, err := h.resolver.Resource(req.Context(), name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "mkvhttp: resource not found", http.StatusNotFound)
			return
		}
		http.Error(w, "mkvhttp: resolver error", http.StatusBadGateway)
		return
	}

	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	if inm := req.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", cacheControlFor(name))

	http.ServeContent(w, req, name, time.Time{}, bytes.NewReader(data))
}

func (h *handler) methodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "mkvhttp: method not allowed", http.StatusMethodNotAllowed)
}

// etagMatches reports whether etag appears in the (possibly comma-separated)
// If-None-Match header value, or that value is the wildcard "*".
func etagMatches(header, etag string) bool {
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		if strings.TrimSpace(part) == etag {
			return true
		}
	}
	return false
}

// cacheControlFor returns the Cache-Control value for a resource name; see
// the Handler doc for the reasoning.
func cacheControlFor(name string) string {
	if strings.HasSuffix(name, ".m3u8") || strings.HasSuffix(name, ".mpd") {
		return "no-cache"
	}
	return "public, max-age=31536000, immutable"
}
