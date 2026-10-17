package httpfs

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

// redirectPair is an origin that redirects every request to a target recording what reaches it.
func redirectPair(t *testing.T) (origin string, hits *atomic.Int64, seen *atomic.Value, targetHost string) {
	t.Helper()
	hits, seen = &atomic.Int64{}, &atomic.Value{}
	seen.Store(http.Header{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		seen.Store(r.Header.Clone())
		w.Header().Set("Content-Range", "bytes 0-3/4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("abcd"))
	}))
	t.Cleanup(target.Close)
	o := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(o.Close)
	u, _ := url.Parse(target.URL)
	return o.URL + "/o.bin", hits, seen, u.Host
}

func readFirst(fs *FS, u string) error {
	f, err := fs.Port().DoOpen(u)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.ReadFull(f, make([]byte, 4))
	return err
}

func TestSEC04_HeadersNotSentAcrossHosts(t *testing.T) {
	origin, hits, seen, _ := redirectPair(t)
	fs := New(Options{Header: http.Header{"Authorization": {"Bearer s"}, "X-Api-Key": {"k"}}})
	if err := readFirst(fs, origin); err == nil {
		t.Error("a redirect to another host was followed with the caller's headers")
	}
	if n := hits.Load(); n != 0 {
		h := seen.Load().(http.Header)
		t.Fatalf("the other host got %d requests (Authorization %q, X-Api-Key %q)", n, h.Get("Authorization"), h.Get("X-Api-Key"))
	}
}

func TestSEC04_RedirectWithoutHeadersFollowed(t *testing.T) {
	origin, hits, _, _ := redirectPair(t)
	if err := readFirst(New(), origin); err != nil {
		t.Fatalf("a header-free redirect is refused: %v", err)
	}
	if hits.Load() == 0 {
		t.Fatal("the redirect was not followed")
	}
}

func TestSEC04_AllowHost(t *testing.T) {
	origin, hits, seen, targetHost := redirectPair(t)
	originHost := mustHost(t, origin)
	fs := New(Options{Header: http.Header{"X-Api-Key": {"k"}}, AllowHost: func(h string) bool { return h == originHost || h == targetHost }})
	if err := readFirst(fs, origin); err != nil {
		t.Fatalf("a redirect to an allowed host is refused: %v", err)
	}
	if hits.Load() == 0 || seen.Load().(http.Header).Get("X-Api-Key") != "k" {
		t.Fatal("the allowed host was not reached with the headers")
	}
	deny := New(Options{AllowHost: func(h string) bool { return h == originHost }})
	hits.Store(0)
	if err := readFirst(deny, origin); err == nil || hits.Load() != 0 {
		t.Fatalf("a redirect to a host AllowHost refuses was followed (err %v, %d hits)", err, hits.Load())
	}
	if err := readFirst(New(Options{AllowHost: func(string) bool { return false }}), origin); err == nil {
		t.Fatal("a URL whose host AllowHost refuses was requested")
	}
}

func mustHost(t *testing.T, s string) string {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}
