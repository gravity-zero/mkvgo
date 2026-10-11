package httpfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

// rawRange answers a ranged GET with whatever body and headers fn decides, for the malformed cases.
func rawRange(t *testing.T, fn func(w http.ResponseWriter, a, b int64)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spec := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
		x, y, _ := strings.Cut(spec, "-")
		a, _ := strconv.ParseInt(x, 10, 64)
		b, _ := strconv.ParseInt(y, 10, 64)
		fn(w, a, b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/o.bin"
}

func readAt(t *testing.T, url string, window int, off int64, n int) ([]byte, error) {
	t.Helper()
	f, err := New(Options{WindowSize: window}).Port().DoOpen(url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, n)
	m, err := io.ReadFull(f, buf)
	return buf[:m], err
}

// A 206 is taken only when its Content-Range and its body agree with what was
// asked: the start, an end within the request, a total, and exactly that many bytes.
func TestFetchRefusesIncoherentPartials(t *testing.T) {
	const total = 1000
	body := func(a, b int64) []byte {
		out := make([]byte, b-a+1)
		for i := range out {
			out[i] = byte(a + int64(i))
		}
		return out
	}
	cases := map[string]func(w http.ResponseWriter, a, b int64){
		"body shorter than the range (no Content-Length)": func(w http.ResponseWriter, a, b int64) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, total))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(body(a, b)[:10])
			w.(http.Flusher).Flush() // chunked: the client cannot tell from the framing
		},
		"body longer than the range": func(w http.ResponseWriter, a, b int64) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, total))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(body(a, b))
			w.Write([]byte("trailing garbage"))
			w.(http.Flusher).Flush()
		},
		"end past the request": func(w http.ResponseWriter, a, b int64) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b+50, total))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(body(a, b+50))
		},
		"end before the start": func(w http.ResponseWriter, a, b int64) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, a-1, total))
			w.WriteHeader(http.StatusPartialContent)
		},
		"end past the total": func(w http.ResponseWriter, a, b int64) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, b))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(body(a, b))
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := readAt(t, rawRange(t, fn), 100, 200, 100)
			if err == nil {
				t.Fatalf("accepted %d bytes", len(got))
			}
		})
	}
}

// A total that moves between two windows of one reader is another version.
func TestFetchTotalChangeIsSourceChanged(t *testing.T) {
	var calls atomic.Int32
	url := rawRange(t, func(w http.ResponseWriter, a, b int64) {
		total := int64(1000)
		if calls.Add(1) > 1 {
			total = 2000
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, total))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(make([]byte, b-a+1))
	})
	_, err := readAt(t, url, 100, 0, 150)
	if !errors.Is(err, mkv.ErrSourceChanged) {
		t.Fatalf("err = %v, want mkv.ErrSourceChanged", err)
	}
}

// A reader pinned to a validator refuses a response that omits it: nothing proves it is that version.
func TestPinnedReadRefusesResponseWithoutValidator(t *testing.T) {
	for _, c := range []struct{ name, pin string }{{"etag", `"v1"`}, {"last-modified", "Sun, 11 Oct 2026 12:00:00 GMT"}} {
		t.Run(c.name, func(t *testing.T) {
			url := rawRange(t, func(w http.ResponseWriter, a, b int64) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/1000", a, b))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(make([]byte, b-a+1))
			})
			f, _ := New(Options{WindowSize: 100}).Port().DoOpen(url)
			f.(mkv.PinnedReader).Pin(c.pin)
			if _, err := f.Read(make([]byte, 10)); err == nil {
				t.Fatal("a response without the pinned validator was accepted")
			}
		})
	}
}

// The identity says how strong it is: a strong ETag is, Last-Modified and a weak ETag are not.
func TestIdentityStrength(t *testing.T) {
	for _, c := range []struct {
		name, etag string
		lm         bool
		id         string
		strong     bool
	}{
		{"strong etag", `"abc"`, true, `"abc"`, true},
		{"last-modified only", "", true, "Sun, 11 Oct 2026 12:00:00 GMT", false},
		{"weak etag only", `W/"abc"`, false, `W/"abc"`, false},
		{"weak etag and last-modified", `W/"abc"`, true, "Sun, 11 Oct 2026 12:00:00 GMT", false},
		{"nothing", "", false, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.etag != "" {
					w.Header().Set("ETag", c.etag)
				}
				if c.lm {
					w.Header().Set("Last-Modified", "Sun, 11 Oct 2026 12:00:00 GMT")
				}
				w.Header().Set("Content-Length", "1000")
			}))
			defer srv.Close()
			fi, err := New().Port().DoStat(srv.URL + "/o")
			if err != nil {
				t.Fatal(err)
			}
			id := fi.(mkv.FileIdentity)
			if id.Identity() != c.id || id.Strong() != c.strong {
				t.Fatalf("identity %q strong %v, want %q %v", id.Identity(), id.Strong(), c.id, c.strong)
			}
		})
	}
}

// hangingServer accepts requests and never answers; released reports each request whose context ended.
func hangingServer(t *testing.T) (url string, released chan struct{}) {
	t.Helper()
	released = make(chan struct{}, 16)
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			released <- struct{}{}
		case <-stop:
		}
	}))
	t.Cleanup(func() { close(stop); srv.Close() })
	return srv.URL + "/o.bin", released
}

// A request to a server that never answers ends at RequestTimeout.
func TestRequestTimeout(t *testing.T) {
	url, released := hangingServer(t)
	f, _ := New(Options{RequestTimeout: 100 * time.Millisecond}).Port().DoOpen(url)
	done := make(chan error, 1)
	go func() { _, err := f.Read(make([]byte, 10)); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a hung request read successfully")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the read is still blocked after 3 s with a 100 ms timeout")
	}
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("the server never saw the request end")
	}
}

// A reader opened with a context abandons its request in flight when the context ends.
func TestOpenContextCancelsRequestInFlight(t *testing.T) {
	url, released := hangingServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	f, err := New(Options{RequestTimeout: -1}).Port().DoOpenContext(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.Read(make([]byte, 10)); done <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the read is still blocked 3 s after its context ended")
	}
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("the server never saw the request end")
	}
}
