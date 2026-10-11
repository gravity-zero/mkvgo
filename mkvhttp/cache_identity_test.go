package mkvhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mp4"
)

// Segment names do not carry the source's version: by default they are
// revalidated, and immutable is the caller's explicit choice.
func TestSegmentsRevalidateUnlessImmutable(t *testing.T) {
	src := streamFixture(t)
	plan, err := mp4.PlanHLS(context.Background(), src, mp4.Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "f.mkv")
	if err := os.WriteFile(file, []byte("direct play"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		immutable bool
		h         http.Handler
		path      string
	}{
		{"streamed", false, Handler(plan), "/seg00002.m4s"},
		{"buffered", false, Handler(plan, Options{Buffered: true}), "/seg00002.m4s"},
		{"init", false, Handler(plan), "/init.mp4"},
		{"file", false, FileHandler(file), "/"},
		{"streamed immutable", true, Handler(plan, Options{Immutable: true}), "/seg00002.m4s"},
		{"init immutable", true, Handler(plan, Options{Immutable: true}), "/init.mp4"},
		{"file immutable", true, FileHandler(file, Options{Immutable: true}), "/"},
	} {
		srv := httptest.NewServer(c.h)
		resp := get(t, srv.URL+c.path, nil)
		resp.Body.Close()
		srv.Close()
		cc := resp.Header.Get("Cache-Control")
		if got := strings.Contains(cc, "immutable"); got != c.immutable {
			t.Errorf("%s: Cache-Control %q", c.name, cc)
		}
		if !c.immutable && cc != "no-cache" {
			t.Errorf("%s: Cache-Control %q, want no-cache", c.name, cc)
		}
	}
	srv := httptest.NewServer(Handler(plan, Options{Immutable: true}))
	defer srv.Close()
	if resp := get(t, srv.URL+"/playlist.m3u8", nil); resp.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("a playlist must never be immutable: %q", resp.Header.Get("Cache-Control"))
	}
}

// A file's ETag is strong (size, mtime, file identity) and answers a conditional GET.
func TestFileHandlerETagIsStrong(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f.mkv")
	if err := os.WriteFile(file, []byte("direct play"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(FileHandler(file))
	defer srv.Close()
	resp := get(t, srv.URL+"/", nil)
	resp.Body.Close()
	if etag := resp.Header.Get("ETag"); !strings.HasPrefix(etag, `"`) {
		t.Fatalf("ETag %q, want a strong ETag", etag)
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		t.Errorf("Last-Modified %q: a date validator cannot tell apart two versions within one second", lm)
	}
	r2 := get(t, srv.URL+"/", map[string]string{"If-None-Match": resp.Header.Get("ETag")})
	r2.Body.Close()
	if r2.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match on the ETag answered %d", r2.StatusCode)
	}
}
