package s3fs

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestConcurrentReadsReuseConnections: 16 readers at once reuse their connections to the endpoint instead of opening one
// per read past the 2 idle ones http.DefaultTransport keeps.
func TestConcurrentReadsReuseConnections(t *testing.T) {
	var mu sync.Mutex
	conns := map[string]bool{}
	srv := rangeServer(t, []byte(strings.Repeat("x", 64<<10)), func(r *http.Request) {
		mu.Lock()
		conns[r.RemoteAddr] = true
		mu.Unlock()
	})
	defer srv.Close()
	port := New(Options{Region: "us-east-1", Endpoint: srv.URL, PathStyle: true, AccessKey: "AKID", SecretKey: "SECRET"}).Port()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 20 {
				f, err := port.DoOpen("s3://mybucket/movie.mkv")
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
	if n := len(conns); n > 32 {
		t.Fatalf("%d connections for 320 reads by 16 readers", n)
	}
}
