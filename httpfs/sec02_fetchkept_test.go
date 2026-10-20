package httpfs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/costguard"
	"github.com/gravity-zero/mkvgo/mkv"
)

const sec02Total = int64(1) << 40

func statShared(t *testing.T, url string) (os.FileInfo, error) {
	t.Helper()
	f, err := New().Port().DoOpenContext(mkv.ShareReads(context.Background()), url)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, ok := f.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		t.Fatal("httpfs reader has no Stat")
	}
	return st.Stat()
}

func TestSEC02_FetchKeptBounds(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, a, b int64){
		"whole object for any range": func(w http.ResponseWriter, a, b int64) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", sec02Total-1, sec02Total))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, 10))
		},
		"head past the window": func(w http.ResponseWriter, a, b int64) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", 8<<20, sec02Total))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, 10))
		},
		"content-length off the range": func(w http.ResponseWriter, a, b int64) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-9/%d", sec02Total))
			w.Header().Set("Content-Length", "5")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, 5))
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			url := rawRange(t, fn)
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			before := m.TotalAlloc
			_, err := statShared(t, url)
			runtime.ReadMemStats(&m)
			if err == nil {
				t.Fatal("Stat took a response that does not answer the range asked")
			}
			if grew := m.TotalAlloc - before; grew > 1<<20 {
				t.Fatalf("Stat allocated %d KiB on a response serving 10 bytes", grew>>10)
			}
		})
	}
}

func TestSEC02_FetchKeptSuffixNotAtEnd(t *testing.T) {
	url := rawRange(t, func(w http.ResponseWriter, a, b int64) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-9/%d", sec02Total))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 10))
	})
	f, err := New().Port().DoOpenContext(mkv.ShareReads(context.Background()), url)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := f.(*reader)
	if _, err := r.Stat(); err != nil {
		t.Fatal(err)
	}
	for _, e := range r.share.recent {
		if e.off != 0 {
			t.Fatalf("kept a tail at %d", e.off)
		}
	}
	if len(r.share.recent) != 1 {
		t.Fatalf("kept %d ranges, want the head alone: a suffix answered by bytes 0-9 is not the tail", len(r.share.recent))
	}
}

// FuzzHTTPFSResponse: whatever status, Content-Range, Content-Length and body a server sends, a reader neither panics nor allocates past its windows.
func FuzzHTTPFSResponse(f *testing.F) {
	f.Add(206, "bytes 0-1099511627775/1099511627776", "", []byte("0123456789"))
	f.Add(206, "bytes 0-9/10", "10", []byte("0123456789"))
	f.Add(206, "bytes 0-9/1099511627776", "5", []byte("01234"))
	f.Add(200, "", "", []byte("x"))
	f.Add(416, "bytes */0", "", []byte{})
	var mu sync.Mutex
	var status int
	var cr, cl string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		st, crv, clv, b := status, cr, cl, body
		mu.Unlock()
		if crv != "" {
			w.Header().Set("Content-Range", crv)
		}
		if clv != "" {
			w.Header().Set("Content-Length", clv)
		}
		w.Header().Set("ETag", `"f"`)
		w.WriteHeader(st)
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	f.Fuzz(func(t *testing.T, st int, crv, clv string, b []byte) {
		costguard.Fuzz(t, len(crv)+len(clv)+len(b), func() {
			if st < 200 || st > 599 || len(b) > 1<<20 || strings.ContainsAny(crv+clv, "\r\n\x00") {
				return
			}
			mu.Lock()
			status, cr, cl, body = st, crv, clv, b
			mu.Unlock()
			fs := New(Options{WindowSize: 64 << 10})
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			before := m.TotalAlloc
			if r, err := fs.Port().DoOpenContext(mkv.ShareReads(context.Background()), srv.URL+"/f"); err == nil {
				if st, ok := r.(interface{ Stat() (os.FileInfo, error) }); ok {
					_, _ = st.Stat()
				}
				_, _ = r.Seek(0, io.SeekEnd)
				_, _ = r.Seek(0, io.SeekStart)
				_, _ = io.ReadFull(r, make([]byte, 4096))
				r.Close()
			}
			runtime.ReadMemStats(&m)
			if grew := m.TotalAlloc - before; grew > 8<<20 {
				t.Fatalf("a %d response with Content-Range %q allocated %d KiB", st, crv, grew>>10)
			}
		})
	})
}
