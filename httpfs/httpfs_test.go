package httpfs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/matroska"
)

// serveFixture serves the file with Range support and counts requests.
func serveFixture(t *testing.T, path string, requests *int) *httptest.Server {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests++
		http.ServeContent(w, r, "f.mkv", time.Time{}, strings.NewReader(string(data)))
	}))
}

// A remote MKV is probed head-only: the metadata arrives correct while the
// bytes transferred stay a fraction of the file.
func TestOpenMetaOverHTTP(t *testing.T) {
	var requests int
	srv := serveFixture(t, "../internal/testdata/regfix.mkv", &requests)
	defer srv.Close()

	f := New(Options{WindowSize: 64 << 10})
	c, err := matroska.OpenMetaWithFS(context.Background(), srv.URL+"/f.mkv", f.Port())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tracks) == 0 || c.DurationMs <= 0 {
		t.Fatalf("remote probe incomplete: %d tracks, %dms", len(c.Tracks), c.DurationMs)
	}
	st, err := os.Stat("../internal/testdata/regfix.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if f.BytesFetched() >= st.Size() {
		t.Errorf("probe fetched %d bytes of a %d-byte file - not head-only", f.BytesFetched(), st.Size())
	}
	t.Logf("probe: %d bytes fetched of %d (%d requests)", f.BytesFetched(), st.Size(), requests)
}

// Reads and seeks behave like a file: sequential reads within one window make
// one request, EOF is honoured, SeekEnd learns the size.
func TestReaderSemantics(t *testing.T) {
	data := []byte(strings.Repeat("0123456789", 100))
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.ServeContent(w, r, "d", time.Time{}, strings.NewReader(string(data)))
	}))
	defer srv.Close()

	fs := New(Options{WindowSize: 256}).Port()
	r, err := fs.DoOpen(srv.URL + "/d")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	head := make([]byte, 10)
	if _, err := io.ReadFull(r, head); err != nil || string(head) != "0123456789" {
		t.Fatalf("head = %q, err %v", head, err)
	}
	if _, err := io.ReadFull(r, head); err != nil || requests != 1 {
		t.Fatalf("second read within the window made a request (total %d)", requests)
	}
	if n, err := r.Seek(-4, io.SeekEnd); err != nil || n != int64(len(data)-4) {
		t.Fatalf("SeekEnd = %d, %v", n, err)
	}
	tail, err := io.ReadAll(r)
	if err != nil || string(tail) != "6789" {
		t.Fatalf("tail = %q, %v", tail, err)
	}
	if _, err := r.Read(head); err != io.EOF {
		t.Fatalf("read past EOF = %v, want EOF", err)
	}

	if st, err := fs.DoStat(srv.URL + "/d"); err != nil || st.Size() != int64(len(data)) {
		t.Fatalf("stat = %v, %v", st, err)
	}
}

// A server ignoring Range gets a refusal, not a silent full download; writes
// are rejected; non-URLs are rejected.
func TestRejections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // ignores Range
		_, _ = w.Write([]byte("full body"))
	}))
	defer srv.Close()

	fs := New().Port()
	r, err := fs.DoOpen(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 4)); err == nil || !strings.Contains(err.Error(), "Range") {
		t.Errorf("no-Range server: err = %v, want Range refusal", err)
	}
	if _, err := fs.DoOpen("/local/path"); err == nil {
		t.Error("non-URL path must be rejected")
	}
	if _, err := fs.DoCreate("http://x/y"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("create: err = %v, want read-only", err)
	}
}
