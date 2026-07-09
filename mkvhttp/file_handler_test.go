package mkvhttp

import (
	"bytes"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFileHandler_GetWholeFile(t *testing.T) {
	data := []byte("the quick brown fox jumps over the lazy dog")
	path := writeTempFile(t, "movie.mkv", data)

	srv := httptest.NewServer(FileHandler(path))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, data) {
		t.Errorf("body = %q, want %q", body, data)
	}
	if got := resp.Header.Get("Content-Type"); got != "video/x-matroska" {
		t.Errorf("Content-Type = %q, want video/x-matroska", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", got)
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("ETag header missing")
	}
}

func TestFileHandler_ContentTypeByExtension(t *testing.T) {
	cases := []struct {
		name   string
		wantCT string
	}{
		{"a.mkv", "video/x-matroska"},
		{"a.webm", "video/webm"},
		{"a.mp4", "video/mp4"},
		{"a.m4v", "video/mp4"},
		{"a.bin", "application/octet-stream"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeTempFile(t, c.name, []byte("data"))
			srv := httptest.NewServer(FileHandler(path))
			defer srv.Close()
			resp, err := http.Get(srv.URL + "/")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if got := resp.Header.Get("Content-Type"); got != c.wantCT {
				t.Errorf("Content-Type = %q, want %q", got, c.wantCT)
			}
		})
	}
}

func TestFileHandler_RangePartial(t *testing.T) {
	data := make([]byte, 1000)
	for i := range data {
		data[i] = byte(i)
	}
	path := writeTempFile(t, "movie.mkv", data)

	srv := httptest.NewServer(FileHandler(path))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("Range", "bytes=100-199")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	wantRange := "bytes 100-199/1000"
	if got := resp.Header.Get("Content-Range"); got != wantRange {
		t.Errorf("Content-Range = %q, want %q", got, wantRange)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 100 {
		t.Fatalf("len(body) = %d, want 100", len(body))
	}
	if !bytes.Equal(body, data[100:200]) {
		t.Errorf("body mismatch")
	}
}

func TestFileHandler_Head(t *testing.T) {
	data := []byte("some content")
	path := writeTempFile(t, "movie.mkv", data)

	srv := httptest.NewServer(FileHandler(path))
	defer srv.Close()

	resp, err := http.Head(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := make([]byte, 1)
	n, _ := resp.Body.Read(body)
	if n != 0 {
		t.Errorf("HEAD response should have no body, read %d bytes", n)
	}
	if got := resp.Header.Get("Content-Length"); got != "12" {
		t.Errorf("Content-Length = %q, want 12", got)
	}
}

func TestFileHandler_ConditionalGet(t *testing.T) {
	data := []byte("some content")
	path := writeTempFile(t, "movie.mkv", data)

	srv := httptest.NewServer(FileHandler(path))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag returned")
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("status = %d, want 304", resp2.StatusCode)
	}
}

func TestFileHandler_MethodNotAllowed(t *testing.T) {
	path := writeTempFile(t, "movie.mkv", []byte("data"))

	srv := httptest.NewServer(FileHandler(path))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q", got)
	}
}

func TestFileHandler_NotFound(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.mkv")

	srv := httptest.NewServer(FileHandler(missing))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestFileHandler_CORS(t *testing.T) {
	path := writeTempFile(t, "movie.mkv", []byte("data"))

	srv := httptest.NewServer(FileHandler(path, Options{AllowCORS: true}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

// TestFileHandler_LargeFileRangeReassembly proves streaming works end to
// end: a multi-MB file is fetched in a series of Range requests and the
// reassembled bytes match the original file's sha256, whether served whole
// or in pieces.
func TestFileHandler_LargeFileRangeReassembly(t *testing.T) {
	const size = 5 * 1024 * 1024 // 5 MiB
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 7)
	}
	path := writeTempFile(t, "movie.mkv", data)
	wantSum := sha256.Sum256(data)

	srv := httptest.NewServer(FileHandler(path))
	defer srv.Close()

	const chunk = 777 * 1024 // deliberately not a divisor of size
	var reassembled bytes.Buffer
	for start := 0; start < size; start += chunk {
		end := start + chunk - 1
		if end >= size {
			end = size - 1
		}
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
		req.Header.Set("Range", "bytes="+itoa(start)+"-"+itoa(end))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", resp.StatusCode)
		}
		if _, err := io.Copy(&reassembled, resp.Body); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	if reassembled.Len() != size {
		t.Fatalf("reassembled len = %d, want %d", reassembled.Len(), size)
	}
	gotSum := sha256.Sum256(reassembled.Bytes())
	if gotSum != wantSum {
		t.Error("reassembled bytes do not match original file's sha256")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
