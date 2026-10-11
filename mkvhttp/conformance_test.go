package mkvhttp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mp4"
)

func get(t *testing.T, url string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A Range under an If-Range that does not strongly match the ETag is ignored:
// the whole resource comes back as 200. A weak ETag never matches strongly.
func TestStreamedIfRangeMismatchServesWhole(t *testing.T) {
	src := streamFixture(t)
	plan, err := mp4.PlanHLS(context.Background(), src, mp4.Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(plan))
	defer srv.Close()
	want, _, _ := plan.Resource(context.Background(), "seg00002.m4s")
	etag := get(t, srv.URL+"/seg00002.m4s", nil).Header.Get("ETag")
	if !strings.HasPrefix(etag, `W/"`) {
		t.Fatalf("a streamed segment carries a weak ETag, got %q", etag)
	}
	for _, ifRange := range []string{`"other"`, etag, "Sat, 29 Oct 1994 19:43:31 GMT"} {
		resp := get(t, srv.URL+"/seg00002.m4s", map[string]string{"Range": "bytes=0-99", "If-Range": ifRange})
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body, want) {
			t.Errorf("If-Range %q: status %d, %d bytes, want 200 and %d bytes", ifRange, resp.StatusCode, len(body), len(want))
		}
	}
	resp := get(t, srv.URL+"/seg00002.m4s", map[string]string{"Range": "bytes=0-99"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, want[:100]) {
		t.Errorf("plain Range: status %d, %d bytes", resp.StatusCode, len(body))
	}
}

// If-None-Match uses the weak comparison: the tag with or without its W/ prefix matches.
func TestIfNoneMatchWeakComparison(t *testing.T) {
	src := streamFixture(t)
	plan, err := mp4.PlanHLS(context.Background(), src, mp4.Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(plan))
	defer srv.Close()
	for _, name := range []string{"seg00002.m4s", "init.mp4"} {
		etag := get(t, srv.URL+"/"+name, nil).Header.Get("ETag")
		other := strings.TrimPrefix(etag, "W/")
		if other == etag {
			other = "W/" + etag
		}
		for _, inm := range []string{etag, other, `"x", ` + other, "*"} {
			resp := get(t, srv.URL+"/"+name, map[string]string{"If-None-Match": inm})
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotModified {
				t.Errorf("%s: If-None-Match %s against ETag %s answered %d", name, inm, etag, resp.StatusCode)
			}
		}
		resp := get(t, srv.URL+"/"+name, map[string]string{"If-None-Match": `"nope"`})
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: a foreign tag answered %d", name, resp.StatusCode)
		}
	}
}

// A 304 carries the Cache-Control a 200 would (RFC 9110 15.4.5), on every handler.
func Test304CarriesCacheControl(t *testing.T) {
	src := streamFixture(t)
	plan, err := mp4.PlanHLS(context.Background(), src, mp4.Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "f.mkv")
	if err := os.WriteFile(file, []byte("not really"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		h    http.Handler
		path string
	}{
		{"streamed", Handler(plan), "/seg00002.m4s"},
		{"buffered", Handler(plan, Options{Buffered: true}), "/seg00002.m4s"},
		{"playlist", Handler(plan), "/playlist.m3u8"},
		{"file", FileHandler(file), "/"},
	} {
		srv := httptest.NewServer(c.h)
		first := get(t, srv.URL+c.path, nil)
		first.Body.Close()
		resp := get(t, srv.URL+c.path, map[string]string{"If-None-Match": first.Header.Get("ETag")})
		resp.Body.Close()
		srv.Close()
		if resp.StatusCode != http.StatusNotModified {
			t.Errorf("%s: status %d", c.name, resp.StatusCode)
			continue
		}
		if got, want := resp.Header.Get("Cache-Control"), first.Header.Get("Cache-Control"); got != want || got == "" {
			t.Errorf("%s: 304 Cache-Control %q, 200 had %q", c.name, got, want)
		}
		if resp.Header.Get("ETag") != first.Header.Get("ETag") {
			t.Errorf("%s: 304 ETag %q", c.name, resp.Header.Get("ETag"))
		}
	}
}
