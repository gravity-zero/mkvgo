package mkvhttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gravity-zero/mkvgo/mp4"
)

// A resource held whole counts what the response carries: a range its part, a whole GET all of it.
func TestHandlerCountsWhatAWholeResourceSends(t *testing.T) {
	plan, err := mp4.PlanHLS(context.Background(), streamFixture(t), mp4.Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(plan))
	defer srv.Close()
	get := func(rng string) (int64, int) {
		t.Helper()
		before := plan.Stats().StreamedBytes
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/init.mp4", nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if got := plan.Stats().StreamedBytes - before; got != int64(len(body)) {
			t.Errorf("Range %q: %d bytes sent, %d counted", rng, len(body), got)
		}
		return int64(len(body)), resp.StatusCode
	}
	if n, status := get("bytes=0-9"); n != 10 || status != http.StatusPartialContent {
		t.Errorf("a 10-byte range: %d bytes, status %d", n, status)
	}
	if n, status := get(""); n == 0 || status != http.StatusOK {
		t.Errorf("a whole GET: %d bytes, status %d", n, status)
	}
}
