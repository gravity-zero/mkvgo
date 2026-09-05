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

// rangeServer serves body with Range support, recording every Range header it
// saw so a test can assert what was actually asked for.
func rangeServer(t *testing.T, body []byte, ranges *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ranges != nil {
			*ranges = append(*ranges, r.Header.Get("Range"))
		}
		http.ServeContent(w, r, "f.bin", time.Time{}, strings.NewReader(string(body)))
	}))
}

// A server that answers 206 but cannot say how big the file is leaves the
// reader with no size to bound anything against; it must be refused, not
// treated as "size unknown, carry on".
func TestFetchRejectsUnusableContentRange(t *testing.T) {
	for _, cr := range []string{"", "bytes 0-3/*", "bytes 0-3/not-a-number", "garbage"} {
		t.Run("Content-Range="+cr, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if cr != "" {
					w.Header().Set("Content-Range", cr)
				}
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("abcd"))
			}))
			defer srv.Close()

			f, err := New().Port().DoOpen(srv.URL + "/x")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Read(make([]byte, 4)); err == nil {
				t.Fatal("a 206 with no usable total was accepted")
			} else if !strings.Contains(err.Error(), "Content-Range") {
				t.Errorf("err = %v, want it to name Content-Range", err)
			}
		})
	}
}

// 416 means "you asked past the end". With a known size that is a clean EOF;
// with no size learned yet it is the server refusing the very first probe, and
// must surface as an error rather than an empty file.
func TestFetchRangeNotSatisfiable(t *testing.T) {
	t.Run("no size known yet", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		}))
		defer srv.Close()
		f, err := New().Port().DoOpen(srv.URL + "/x")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Read(make([]byte, 4)); err == nil {
			t.Fatal("a 416 on the first fetch was reported as an empty read")
		}
	})

	t.Run("size known: clean EOF", func(t *testing.T) {
		body := []byte("0123456789")
		srv := rangeServer(t, body, nil)
		defer srv.Close()
		f, err := New().Port().DoOpen(srv.URL + "/x")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(f); err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if _, err := f.Seek(int64(len(body)), io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if n, err := f.Read(make([]byte, 4)); n != 0 || err != io.EOF {
			t.Errorf("read at EOF = (%d, %v), want (0, io.EOF)", n, err)
		}
	})
}

// Any other status is an error that names it - never a silent empty read.
func TestFetchSurfacesHTTPStatus(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		f, err := New().Port().DoOpen(srv.URL + "/x")
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.Read(make([]byte, 4))
		if err == nil || !strings.Contains(err.Error(), http.StatusText(code)) {
			t.Errorf("HTTP %d: err = %v, want the status named", code, err)
		}
		srv.Close()
	}
}

// Seek is local except for SeekEnd on a reader that has not learned the size:
// that costs exactly one ranged request, and bad inputs are refused.
func TestSeekSemantics(t *testing.T) {
	body := []byte("0123456789abcdef")
	var ranges []string
	srv := rangeServer(t, body, &ranges)
	defer srv.Close()

	f, err := New().Port().DoOpen(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(2, io.SeekCurrent); err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 0 {
		t.Errorf("a local seek issued %d request(s): %v", len(ranges), ranges)
	}
	// SeekEnd must learn the size, and land at the right place.
	end, err := f.Seek(-4, io.SeekEnd)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len(body) - 4); end != want {
		t.Errorf("SeekEnd(-4) = %d, want %d", end, want)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "cdef" {
		t.Errorf("read after SeekEnd(-4) = %q, want %q", got, "cdef")
	}
	if _, err := f.Seek(0, 99); err == nil {
		t.Error("a bad whence was accepted")
	}
	if _, err := f.Seek(-1, io.SeekStart); err == nil {
		t.Error("a negative seek was accepted")
	}
}

// The window bounds every request: a caller reading one byte must not pull the
// whole file, and the accounting must match what the server actually sent.
func TestWindowBoundsRequestsAndAccounting(t *testing.T) {
	body := make([]byte, 4096)
	for i := range body {
		body[i] = byte(i)
	}
	var ranges []string
	srv := rangeServer(t, body, &ranges)
	defer srv.Close()

	fs := New(Options{WindowSize: 512})
	f, err := fs.Port().DoOpen(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 || ranges[0] != "bytes=0-511" {
		t.Fatalf("first read asked %v, want a single bytes=0-511", ranges)
	}
	if n := fs.BytesFetched(); n != 512 {
		t.Errorf("BytesFetched = %d, want 512 (one window)", n)
	}
}

// A caller's headers reach the server: this is how an authenticated source is
// read at all, so it must not be silently dropped.
func TestCustomHeadersAreSent(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		http.ServeContent(w, r, "f.bin", time.Time{}, strings.NewReader("payload"))
	}))
	defer srv.Close()

	h := http.Header{}
	h.Set("Authorization", "Bearer token-123")
	h.Add("X-Trace", "a")
	h.Add("X-Trace", "b")

	f, err := New(Options{Header: h}).Port().DoOpen(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer token-123" {
		t.Errorf("Authorization = %q, want it forwarded", got.Get("Authorization"))
	}
	if v := got.Values("X-Trace"); len(v) != 2 {
		t.Errorf("X-Trace = %v, want both values forwarded", v)
	}
	if got.Get("Range") == "" {
		t.Error("the Range header was lost among the custom ones")
	}
}

// Every write on an HTTP port is refused, not just Create.
func TestPortIsReadOnly(t *testing.T) {
	fs := New().Port()
	checks := map[string]error{}
	_, checks["open-file"] = fs.DoOpenFile("http://x/y", os.O_RDWR, 0)
	checks["mkdir"] = fs.DoMkdirAll("http://x/y", 0)
	checks["write"] = fs.DoWriteFile("http://x/y", nil, 0)
	checks["remove"] = fs.DoRemove("http://x/y")
	_, checks["create"] = fs.DoCreate("http://x/y")
	for op, err := range checks {
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s: err = %v, want a read-only refusal", op, err)
		}
	}
}

// Stat reports the size the server declared, in the shape os.FileInfo callers
// expect from a read-only remote object.
func TestStatFileInfo(t *testing.T) {
	body := []byte("0123456789")
	srv := rangeServer(t, body, nil)
	defer srv.Close()

	url := srv.URL + "/movie.mkv"
	fi, err := New().Port().DoStat(url)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Name() != url {
		t.Errorf("Name = %q, want the URL", fi.Name())
	}
	if fi.Size() != int64(len(body)) {
		t.Errorf("Size = %d, want %d", fi.Size(), len(body))
	}
	if fi.Mode() != 0o444 {
		t.Errorf("Mode = %v, want read-only 0444", fi.Mode())
	}
	if fi.IsDir() {
		t.Error("a remote object reported as a directory")
	}
	if !fi.ModTime().IsZero() {
		t.Errorf("ModTime = %v, want the zero time (none is known)", fi.ModTime())
	}
	if fi.Sys() != nil {
		t.Errorf("Sys = %v, want nil", fi.Sys())
	}
	if _, err := New().Port().DoStat("http://127.0.0.1:1/nope"); err == nil {
		t.Error("Stat of an unreachable URL succeeded")
	}
}

// Hybrid is what a "remux from a URL to a local file" needs: URLs go over HTTP,
// everything else - reads and every write - goes to the OS.
func TestHybridRoutesByScheme(t *testing.T) {
	body := []byte("remote-bytes")
	srv := rangeServer(t, body, nil)
	defer srv.Close()

	fs := Hybrid()
	r, err := fs.DoOpen(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("remote read = %q, want %q", got, body)
	}
	if fi, err := fs.DoStat(srv.URL + "/x"); err != nil {
		t.Fatal(err)
	} else if fi.Size() != int64(len(body)) {
		t.Errorf("remote Stat size = %d, want %d", fi.Size(), len(body))
	}

	// Local side: reads AND writes reach the real filesystem.
	dir := t.TempDir()
	local := dir + "/out.bin"
	w, err := fs.DoCreate(local)
	if err != nil {
		t.Fatalf("Hybrid must allow local writes: %v", err)
	}
	if _, err := w.Write([]byte("local")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	lr, err := fs.DoOpen(local)
	if err != nil {
		t.Fatal(err)
	}
	defer lr.Close()
	lb, err := io.ReadAll(lr)
	if err != nil {
		t.Fatal(err)
	}
	if string(lb) != "local" {
		t.Errorf("local read = %q, want %q", lb, "local")
	}
	if fi, err := fs.DoStat(local); err != nil {
		t.Fatal(err)
	} else if fi.Size() != 5 {
		t.Errorf("local Stat size = %d, want 5", fi.Size())
	}
}
