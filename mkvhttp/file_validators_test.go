package mkvhttp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mp4"
)

// placeFile writes data as path by rename, with mtime at when.
func placeFile(t *testing.T, path string, data []byte, when time.Time) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, when, when); err != nil {
		t.Fatal(err)
	}
	var err error
	for try := 0; try < 50; try++ { // Windows refuses a rename over a file the last response may not have closed yet
		if err = os.Rename(tmp, path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(err)
}

func fill(n int, b byte) []byte { return bytes.Repeat([]byte{b}, n) }

func serveFile(t *testing.T, path string) *httptest.Server {
	srv := httptest.NewServer(FileHandler(path))
	t.Cleanup(srv.Close)
	return srv
}

func body(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return b
}

// A player resuming a range on the ETag it holds gets the range.
func TestFileHandlerRangeIfRangeETag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.mkv")
	placeFile(t, path, fill(4096, 'a'), time.Now().Add(-time.Hour))
	srv := serveFile(t, path)
	etag := get(t, srv.URL+"/", nil).Header.Get("ETag")
	resp := get(t, srv.URL+"/", map[string]string{"Range": "bytes=100-199", "If-Range": etag})
	if b := body(t, resp); resp.StatusCode != http.StatusPartialContent || len(b) != 100 {
		t.Fatalf("Range + If-Range on the ETag %s: status %d, %d bytes, want 206 and 100", etag, resp.StatusCode, len(b))
	}
}

// A file renamed over the served one with the same size and mtime is another
// file: its ETag differs, so neither a 304 nor a range of it answers a client
// holding the old one.
func TestFileHandlerRenamedOverSameStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.mkv")
	when := time.Now().Add(-time.Hour)
	placeFile(t, path, fill(4096, 'a'), when)
	if runtime.GOOS == "windows" {
		t.Skip("no file identity on Windows: a same-stamp replacement keeps the ETag (documented)")
	}
	srv := serveFile(t, path)
	first := get(t, srv.URL+"/", nil)
	body(t, first)
	old := first.Header.Get("ETag")
	placeFile(t, path, fill(4096, 'b'), when)
	if fi, _ := os.Stat(path); !fi.ModTime().Equal(when) {
		t.Skip("the filesystem did not keep the mtime")
	}
	if resp := get(t, srv.URL+"/", map[string]string{"If-None-Match": old}); resp.StatusCode == http.StatusNotModified {
		t.Fatalf("If-None-Match on the old file's ETag answered 304 for another file")
	}
	resp := get(t, srv.URL+"/", map[string]string{"Range": "bytes=0-99", "If-Range": old})
	if b := body(t, resp); resp.StatusCode != http.StatusOK || !bytes.Equal(b, fill(4096, 'b')) {
		t.Fatalf("If-Range on the old ETag: status %d, %d bytes, want the whole new file", resp.StatusCode, len(b))
	}
}

// A date validator cannot tell apart two versions within one second: a
// change within it never answers 304 to If-Modified-Since, nor a range to a
// dated If-Range.
func TestFileHandlerDateValidatorsWithinOneSecond(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.mkv")
	sec := time.Now().Add(-time.Hour).Truncate(time.Second)
	placeFile(t, path, fill(4096, 'a'), sec.Add(100*time.Millisecond))
	srv := serveFile(t, path)
	date := sec.UTC().Format(http.TimeFormat)
	placeFile(t, path, fill(4096, 'b'), sec.Add(600*time.Millisecond))
	if resp := get(t, srv.URL+"/", map[string]string{"If-Modified-Since": date}); resp.StatusCode == http.StatusNotModified {
		t.Errorf("If-Modified-Since %s answered 304 for a version written within that second", date)
	}
	resp := get(t, srv.URL+"/", map[string]string{"Range": "bytes=0-99", "If-Range": date})
	if b := body(t, resp); resp.StatusCode != http.StatusOK || !bytes.Equal(b, fill(4096, 'b')) {
		t.Errorf("dated If-Range %s: status %d, %d bytes, want the whole file", date, resp.StatusCode, len(b))
	}
}

// The plan handler sends no date and so never answers 304 to a date: only its ETag validates.
func TestHandlerIgnoresIfModifiedSince(t *testing.T) {
	src := streamFixture(t)
	plan, err := mp4.PlanHLS(context.Background(), src, mp4.Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(24 * time.Hour).UTC().Format(http.TimeFormat)
	for _, o := range []Options{{}, {Buffered: true}} {
		srv := httptest.NewServer(Handler(plan, o))
		for _, name := range []string{"seg00002.m4s", "init.mp4", "playlist.m3u8"} {
			resp := get(t, srv.URL+"/"+name, map[string]string{"If-Modified-Since": future})
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Last-Modified") != "" {
				t.Errorf("%s (buffered %v): If-Modified-Since answered %d, Last-Modified %q", name, o.Buffered, resp.StatusCode, resp.Header.Get("Last-Modified"))
			}
		}
		srv.Close()
	}
}
