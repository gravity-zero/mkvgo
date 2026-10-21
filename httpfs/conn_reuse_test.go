package httpfs

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Sixteen readers reading at once keep their connections: the default client
// opens about one per reader, not one for most requests past the second.
func TestConcurrentReadsReuseConnections(t *testing.T) {
	body := strings.Repeat("x", 64<<10)
	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f", time.Unix(1700000000, 0), strings.NewReader(body))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	fs := New()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 20 {
				f, err := fs.open(t.Context(), srv.URL+"/f")
				if err != nil {
					t.Error(err)
					return
				}
				buf := make([]byte, 1024)
				if _, err := f.(io.ReaderAt).ReadAt(buf, int64(i*1024)); err != nil {
					t.Error(err)
				}
				f.Close()
			}
		}()
	}
	wg.Wait()
	if n := conns.Load(); n > 32 {
		t.Fatalf("%d connections for 320 reads by 16 readers", n)
	}
}
