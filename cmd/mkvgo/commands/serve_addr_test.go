package commands

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestServeListensOnLoopbackByDefault pins that serve and serve-growing only open to the network on -addr.
func TestServeListensOnLoopbackByDefault(t *testing.T) {
	if got := parseServeFlags([]string{"movie.mkv"}).addr; got != "127.0.0.1:8478" {
		t.Errorf("serve default addr = %q, want 127.0.0.1:8478", got)
	}
	if got := parseServeGrowingFlags([]string{"movie.mkv"}).addr; got != "127.0.0.1:8478" {
		t.Errorf("serve-growing default addr = %q, want 127.0.0.1:8478", got)
	}
	if got := parseServeFlags([]string{"-addr", ":9000", "movie.mkv"}).addr; got != ":9000" {
		t.Errorf("serve -addr :9000 = %q", got)
	}
}

// TestLoopbackServerChecksTheHost: a server on 127.0.0.1 answered a request naming any host, which a page pointing a
// name it controls at 127.0.0.1 (DNS rebinding) can send and read. It answers localhost and loopback names only; a
// server open to the network is left as it is.
func TestLoopbackServerChecksTheHost(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, c := range []struct {
		addr, host string
		want       int
	}{
		{"127.0.0.1:8478", "127.0.0.1:8478", http.StatusOK},
		{"127.0.0.1:8478", "localhost:8478", http.StatusOK},
		{"127.0.0.1:8478", "[::1]:8478", http.StatusOK},
		{"127.0.0.1:8478", "evil.example:8478", http.StatusMisdirectedRequest},
		{"127.0.0.1:8478", "evil.example", http.StatusMisdirectedRequest},
		{"[::1]:8478", "attacker.test", http.StatusMisdirectedRequest},
		{":8478", "media.lan:8478", http.StatusOK},
		{"0.0.0.0:8478", "media.lan", http.StatusOK},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/master.m3u8", nil)
		req.Host = c.host
		newServer(c.addr, ok, 0).Handler.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("listening on %s, Host %s: %d, want %d", c.addr, c.host, rec.Code, c.want)
		}
	}
}
