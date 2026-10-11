package httpfs

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

// Integration against real HTTP servers serving one directory, opt-in:
// MKVGO_IT_HTTP="nginx=http://127.0.0.1:18080,caddy=http://127.0.0.1:18081"
// MKVGO_IT_HTTP_DIR=<the directory they serve>.
func itServers(t *testing.T) (map[string]string, string) {
	spec, dir := os.Getenv("MKVGO_IT_HTTP"), os.Getenv("MKVGO_IT_HTTP_DIR")
	if spec == "" || dir == "" {
		t.Skip("set MKVGO_IT_HTTP and MKVGO_IT_HTTP_DIR to run against real servers")
	}
	servers := map[string]string{}
	for _, kv := range strings.Split(spec, ",") {
		name, url, _ := strings.Cut(kv, "=")
		servers[name] = url
	}
	return servers, dir
}

// statusLog is a transport that records every response status.
type statusLog struct {
	mu    sync.Mutex
	codes []int
}

func (s *statusLog) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil {
		s.mu.Lock()
		s.codes = append(s.codes, resp.StatusCode)
		s.mu.Unlock()
	}
	return resp, err
}

func (s *statusLog) saw(code int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.codes {
		if c == code {
			return true
		}
	}
	return false
}

// place writes data as name in dir by rename, with mtime at when it is not zero.
func place(t *testing.T, dir, name string, data []byte, when time.Time) {
	t.Helper()
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if !when.IsZero() {
		if err := os.Chtimes(tmp, when, when); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func randomBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func TestIntegrationHTTPServers(t *testing.T) {
	servers, dir := itServers(t)
	const size, window = 3 << 20, 256 << 10
	for name, base := range servers {
		t.Run(name, func(t *testing.T) {
			file := "it_" + name + ".bin"
			url := base + "/" + file
			v1 := randomBytes(size, 1)
			stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
			place(t, dir, file, v1, stamp)

			log := &statusLog{}
			fs := New(Options{WindowSize: window, Client: &http.Client{Transport: log}})
			fi, err := fs.Port().DoStat(url)
			if err != nil {
				t.Fatal(err)
			}
			id := fi.(mkv.FileIdentity)
			if fi.Size() != size || id.Identity() == "" {
				t.Fatalf("stat: size %d, identity %q", fi.Size(), id.Identity())
			}
			t.Logf("validator %s (strong %v), Last-Modified %s", id.Identity(), id.Strong(), fi.ModTime().UTC().Format(time.RFC1123))

			// Range: windows anywhere in the file, exact bytes.
			f, _ := fs.Port().DoOpen(url)
			rng := rand.New(rand.NewSource(2))
			for i := 0; i < 12; i++ {
				off := rng.Int63n(size - 4096)
				buf := make([]byte, 1+rng.Intn(4096))
				f.Seek(off, io.SeekStart)
				if _, err := io.ReadFull(f, buf); err != nil || !bytes.Equal(buf, v1[off:off+int64(len(buf))]) {
					t.Fatalf("range at %d: err %v, equal %v", off, err, bytes.Equal(buf, v1[off:off+int64(len(buf))]))
				}
			}
			tail := make([]byte, 100)
			f.Seek(size-100, io.SeekStart)
			if _, err := io.ReadFull(f, tail); err != nil || !bytes.Equal(tail, v1[size-100:]) {
				t.Fatalf("last window: %v", err)
			}

			// If-Match on another version: 412, read refused.
			g, _ := fs.Port().DoOpen(url)
			g.(mkv.PinnedReader).Pin(`"not-this-version"`)
			if _, err := g.Read(make([]byte, 10)); !errors.Is(err, mkv.ErrSourceChanged) || !log.saw(http.StatusPreconditionFailed) {
				t.Errorf("pinned to another ETag: err %v, 412 seen %v", err, log.saw(http.StatusPreconditionFailed))
			}

			// Replaced mid-read by another file: refused, never mixed.
			r, _ := fs.Port().DoOpen(url)
			if _, err := io.ReadFull(r, make([]byte, 100)); err != nil {
				t.Fatal(err)
			}
			place(t, dir, file, randomBytes(size+4096, 3), time.Time{})
			r.Seek(1<<20, io.SeekStart)
			if _, err := io.ReadFull(r, make([]byte, 100)); !errors.Is(err, mkv.ErrSourceChanged) {
				t.Errorf("replaced by another file mid-read: err %v", err)
			}

			// Replaced by another content of the same size and mtime: measured, not asserted.
			place(t, dir, file, v1, stamp)
			s, _ := fs.Port().DoOpen(url)
			if _, err := io.ReadFull(s, make([]byte, 100)); err != nil {
				t.Fatal(err)
			}
			first, _ := fs.Port().DoStat(url)
			v2 := randomBytes(size, 4)
			place(t, dir, file, v2, stamp)
			second, _ := fs.Port().DoStat(url)
			s.Seek(2<<20, io.SeekStart)
			got := make([]byte, 100)
			_, err = io.ReadFull(s, got)
			outcome := "refused"
			switch {
			case err == nil && bytes.Equal(got, v2[2<<20:2<<20+100]):
				outcome = "MIXED: the reader returned the new file's bytes under the old validator"
			case err == nil:
				outcome = "served the old bytes"
			case !errors.Is(err, mkv.ErrSourceChanged):
				outcome = "error " + err.Error()
			}
			t.Logf("same size and mtime, other content: validator %s -> %s, %s",
				first.(mkv.FileIdentity).Identity(), second.(mkv.FileIdentity).Identity(), outcome)
		})
	}
}
