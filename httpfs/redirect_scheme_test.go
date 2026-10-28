package httpfs

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestRedirectDowngradeRefused: a redirect from https to http on the same host is refused while Options.Header is
// set, since the custom headers would cross the network in clear.
func TestRedirectDowngradeRefused(t *testing.T) {
	from, _ := http.NewRequest("GET", "https://media.example/a.mkv", nil)
	to, _ := http.NewRequest("GET", "http://media.example/a.mkv", nil)
	policy := redirectPolicy(Options{Header: http.Header{"X-Api-Key": {"k"}}})
	if err := policy(to, []*http.Request{from}); err == nil {
		t.Error("an https to http redirect was followed with the caller's headers")
	}
	if err := redirectPolicy(Options{})(to, []*http.Request{from}); err != nil {
		t.Errorf("without headers the downgrade carries nothing of the caller's: %v", err)
	}
}

// TestRedirectSendsNoReferer: net/http sets Referer on a redirect to the URL redirected from, a presigned URL's
// signature included, and sends it to the host redirected to; the default client drops it.
func TestRedirectSendsNoReferer(t *testing.T) {
	var referer atomic.Value
	referer.Store("")
	to := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		referer.Store(r.Header.Get("Referer"))
		w.Header().Set("Content-Length", "4")
		_, _ = w.Write([]byte("data"))
	}))
	defer to.Close()
	from := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, to.URL+"/o.bin", http.StatusFound)
	}))
	defer from.Close()
	if _, err := New().stat(from.URL + "/o.bin?X-Amz-Signature=secret"); err != nil {
		t.Fatal(err)
	}
	if got := referer.Load().(string); got != "" {
		t.Errorf("the host redirected to got Referer %q", got)
	}
}
