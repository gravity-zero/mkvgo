package mkvhttp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// FileHandler serves a single local file over HTTP for direct-play clients:
// GET/HEAD only, full Range support (seeking/scrubbing), a strong ETag, the
// right Content-Type from the extension, and a long-lived immutable cache
// header (the bytes never change). It never reads the whole file into
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
	if inm := req.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", contentTypeFor(h.path))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")

	http.ServeContent(w, req, filepath.Base(h.path), fi.ModTime(), f)
}

// fileETag derives a strong ETag from a file's size and modification time -
// two fields os.Stat already returns for free - hashed together so the
// header stays an opaque, collision-resistant token instead of leaking raw
// inode/mtime bits. This is O(1) in file size: no content is ever read to
// compute it (unlike Handler's resolver ETag, which hashes in-memory bytes
// that a Resolver already built; a source file on disk can be gigabytes,
// so hashing its content here would defeat the point of a fast, streaming
// direct-play handler).
func fileETag(fi os.FileInfo) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d-%d", fi.Size(), fi.ModTime().UnixNano())))
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
