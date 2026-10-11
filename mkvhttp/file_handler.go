package mkvhttp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gravity-zero/mkvgo/internal/fileid"
)

// FileHandler serves a single local file over HTTP for direct-play clients:
// GET/HEAD only, full Range support (seeking/scrubbing), a strong ETag, the
// right Content-Type from the extension, a strong ETag from the size, mtime
// and file identity (not the bytes), no Last-Modified (a date cannot tell
// apart two versions within one second) and "no-cache" (Options.Immutable for
// a URL that names the file's version). It never reads the whole file into
// memory - it streams straight from an *os.File via http.ServeContent, which
// handles Range, Content-Range, 206, If-Range and If-Modified-Since.
//
// The file is opened with os.Open on every request (no FS port indirection):
// Options carries none today, and a port would need to expose both an
// io.ReadSeeker and a known size for http.ServeContent's Range support to
// work, which the resolver-style FS ports in this repository do not
// guarantee - os.Open is the correct, documented fallback for a local path.
//
//	http.Handle("/play/movie.mkv", mkvhttp.FileHandler("movie.mkv"))
func FileHandler(path string, opts ...Options) http.Handler {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	return &fileHandler{path: path, opts: o}
}

type fileHandler struct {
	path string
	opts Options
}

func (h *fileHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
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

	f, err := os.Open(h.path)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "mkvhttp: file not found", http.StatusNotFound)
			return
		}
		http.Error(w, "mkvhttp: file error", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		http.Error(w, "mkvhttp: file not found", http.StatusNotFound)
		return
	}

	etag := fileETag(fi)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", cacheControlFor(h.path, h.opts.Immutable))
	if inm := req.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", contentTypeFor(h.path))

	// No modtime: a date validator (If-Modified-Since, a dated If-Range) cannot tell apart two versions within one second, the ETag can.
	http.ServeContent(w, req, filepath.Base(h.path), time.Time{}, f)
}

// fileETag is a strong ETag from the file's size, mtime to the nanosecond and
// OS identity, in O(1) like nginx's, Apache's and Caddy's, and one step past
// them: a file renamed over the served one changes it even with the same size
// and mtime. Not caught: a rewrite in place that restores the mtime.
func fileETag(fi os.FileInfo) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d-%d-%s", fi.Size(), fi.ModTime().UnixNano(), fileid.Of(fi))))
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// contentTypeFor maps a file extension to the Content-Type FileHandler sets
// before handing off to http.ServeContent, so ServeContent's own
// extension/sniffing detection never overrides it.
func contentTypeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".mp4", ".m4v":
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}
