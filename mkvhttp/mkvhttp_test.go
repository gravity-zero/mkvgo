package mkvhttp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeResolver serves a fixed in-memory resource set, keyed by name.
type fakeResolver struct {
	data map[string][]byte
	ctFn func(name string) string
}

func (f *fakeResolver) Resource(_ context.Context, name string) ([]byte, string, error) {
	d, ok := f.data[name]
	if !ok {
		return nil, "", fmt.Errorf("no such resource %q: %w", name, ErrNotFound)
	}
	return d, f.ctFn(name), nil
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{
		data: map[string][]byte{
			"master.m3u8": []byte("#EXTM3U\n#EXT-X-VERSION:7\n"),
			"init.mp4":    []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p'},
			"seg00001.m4s": func() []byte {
				b := make([]byte, 4096)
				for i := range b {
					b[i] = byte(i)
				}
				return b
			}(),
		},
		ctFn: func(name string) string {
			switch {
			case hasSuffix(name, ".m3u8"):
				return "application/vnd.apple.mpegurl"
			case hasSuffix(name, ".mp4"):
				return "video/mp4"
			default:
				return "video/iso.segment"
			}
		},
	}
}

func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}

func etagOf(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func TestHandler_GetResourceClasses(t *testing.T) {
	res := newFakeResolver()
	h := Handler(res)
	srv := httptest.NewServer(h)
	defer srv.Close()

	cases := []struct {
		name      string
		wantCT    string
		wantCache string
	}{
		{"master.m3u8", "application/vnd.apple.mpegurl", "no-cache"},
		{"init.mp4", "video/mp4", "public, max-age=31536000, immutable"},
		{"seg00001.m4s", "video/iso.segment", "public, max-age=31536000, immutable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + "/" + c.name)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if got := resp.Header.Get("Content-Type"); got != c.wantCT {
				t.Errorf("Content-Type = %q, want %q", got, c.wantCT)
			}
			if got := resp.Header.Get("Cache-Control"); got != c.wantCache {
				t.Errorf("Cache-Control = %q, want %q", got, c.wantCache)
			}
			want := etagOf(res.data[c.name])
			if got := resp.Header.Get("ETag"); got != want {
				t.Errorf("ETag = %q, want %q", got, want)
			}
		})
	}
}

func TestHandler_ConditionalGet(t *testing.T) {
	res := newFakeResolver()
	srv := httptest.NewServer(Handler(res))
	defer srv.Close()

	etag := etagOf(res.data["master.m3u8"])

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/master.m3u8", nil)
	req.Header.Set("If-None-Match", etag)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("status = %d, want 304", resp.StatusCode)
	}
}

func TestHandler_Head(t *testing.T) {
	res := newFakeResolver()
	srv := httptest.NewServer(Handler(res))
	defer srv.Close()

	resp, err := http.Head(srv.URL + "/master.m3u8")
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
}

func TestHandler_RangePartial(t *testing.T) {
	res := newFakeResolver()
	srv := httptest.NewServer(Handler(res))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/seg00001.m4s", nil)
	req.Header.Set("Range", "bytes=10-19")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	buf := make([]byte, 32)
	n, _ := resp.Body.Read(buf)
	if n != 10 {
		t.Fatalf("read %d bytes, want 10", n)
	}
	want := res.data["seg00001.m4s"][10:20]
	for i := range want {
		if buf[i] != want[i] {
			t.Fatalf("byte %d = %d, want %d", i, buf[i], want[i])
		}
	}
}

func TestHandler_NotFound(t *testing.T) {
	res := newFakeResolver()
	srv := httptest.NewServer(Handler(res))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/does-not-exist.m4s")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandler_ResolverErrorIsBadGateway(t *testing.T) {
	res := ResolverFunc(func(_ context.Context, name string) ([]byte, string, error) {
		return nil, "", fmt.Errorf("boom: source unreachable")
	})
	srv := httptest.NewServer(Handler(res))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/anything.m4s")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestHandler_MethodNotAllowed(t *testing.T) {
	res := newFakeResolver()
	srv := httptest.NewServer(Handler(res))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/master.m3u8", "text/plain", nil)
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

func TestHandler_CORS(t *testing.T) {
	res := newFakeResolver()

	t.Run("disabled by default", func(t *testing.T) {
		srv := httptest.NewServer(Handler(res))
		defer srv.Close()
		resp, err := http.Get(srv.URL + "/master.m3u8")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
		}
	})

	t.Run("enabled", func(t *testing.T) {
		srv := httptest.NewServer(Handler(res, Options{AllowCORS: true}))
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/master.m3u8")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
		}

		req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/master.m3u8", nil)
		oresp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer oresp.Body.Close()
		if oresp.StatusCode != http.StatusNoContent {
			t.Errorf("OPTIONS preflight status = %d, want 204", oresp.StatusCode)
		}
	})
}

// TestETagStability_AcrossHandlers proves determinism: two independent
// Handlers built over resolvers serving the identical bytes produce the
// identical ETag for the same resource.
func TestETagStability_AcrossHandlers(t *testing.T) {
	res1 := newFakeResolver()
	res2 := newFakeResolver()

	srv1 := httptest.NewServer(Handler(res1))
	defer srv1.Close()
	srv2 := httptest.NewServer(Handler(res2))
	defer srv2.Close()

	resp1, err := http.Get(srv1.URL + "/init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer resp1.Body.Close()
	resp2, err := http.Get(srv2.URL + "/init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	e1, e2 := resp1.Header.Get("ETag"), resp2.Header.Get("ETag")
	if e1 == "" || e1 != e2 {
		t.Errorf("ETag not stable across handlers: %q vs %q", e1, e2)
	}
}
