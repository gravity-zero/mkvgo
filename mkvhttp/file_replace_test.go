package mkvhttp

import (
	"bytes"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Every response's ETag is the ETag of the bytes it carries: a file renamed
// over the served one between two requests, or while one is being sent,
// never gets the other's ETag, nor a range under the other's.
func TestFileHandlerETagMatchesBytesSent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.mkv")
	when := time.Now().Add(-time.Hour)
	v1, v2 := make([]byte, 8<<20), make([]byte, 8<<20)
	rand.New(rand.NewSource(1)).Read(v1)
	rand.New(rand.NewSource(2)).Read(v2)
	placeFile(t, path, v1, when)
	srv := serveFile(t, path)
	etagOf := map[string][]byte{}
	fetch := func(hdr map[string]string) (*http.Response, []byte) {
		resp := get(t, srv.URL+"/", hdr)
		return resp, body(t, resp)
	}
	resp, b := fetch(nil)
	etagOf[resp.Header.Get("ETag")] = b
	if !bytes.Equal(b, v1) {
		t.Fatal("first response is not v1")
	}

	// Between two requests. Windows has no file identity: v2 differs by its mtime there.
	when2 := when
	if runtime.GOOS == "windows" {
		when2 = when.Add(time.Second)
	}
	placeFile(t, path, v2, when2)
	resp, b = fetch(nil)
	if e := resp.Header.Get("ETag"); !bytes.Equal(b, v2) || etagOf[e] != nil {
		t.Fatalf("after the rename: v2 %v, ETag %s already named other bytes", bytes.Equal(b, v2), e)
	}
	etagOf[resp.Header.Get("ETag")] = b

	// While a response is being sent: the client reads slowly, the file is renamed over midway.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	slow, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 64<<10)
	if _, err := io.ReadFull(slow.Body, head); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp", v1, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path+".tmp", when, when)
	if err := os.Rename(path+".tmp", path); err != nil {
		slow.Body.Close()
		t.Skipf("a file being sent cannot be replaced here (%v)", err)
	}
	rest, err := io.ReadAll(slow.Body)
	slow.Body.Close()
	got := append(head, rest...)
	if err != nil || !bytes.Equal(got, etagOf[slow.Header.Get("ETag")]) {
		t.Fatalf("a response cut across a rename: err %v, its bytes are not the ones its ETag %s names", err, slow.Header.Get("ETag"))
	}

	// A range on the ETag of a file renamed away: the whole current file, never a slice of it under the old name.
	resp, b = fetch(map[string]string{"Range": "bytes=0-99", "If-Range": slow.Header.Get("ETag")})
	if resp.StatusCode != http.StatusOK || !bytes.Equal(b, v1) {
		t.Fatalf("If-Range on a replaced file's ETag: status %d, %d bytes", resp.StatusCode, len(b))
	}
	for e, want := range etagOf {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
		r, b := fetch(map[string]string{"Range": "bytes=0-99", "If-Range": e})
		if r.StatusCode == http.StatusPartialContent && !bytes.Equal(b, want[:100]) {
			t.Fatalf("206 under ETag %s with bytes that are not its", e)
		}
	}
}
