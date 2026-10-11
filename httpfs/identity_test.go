package httpfs

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

// objectServer serves one swappable body with ETag and Last-Modified, counting
// requests by method and the bytes it sent.
type objectServer struct {
	mu      sync.Mutex
	body    []byte
	etag    string
	modTime time.Time
	gets    int
	heads   int
	sent    int64
	ifMatch []string
	noHead  bool
	srv     *httptest.Server
}

func newObjectServer(t *testing.T, body []byte, etag string) *objectServer {
	t.Helper()
	o := &objectServer{body: body, etag: etag, modTime: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		if r.Method == http.MethodHead {
			o.heads++
			if o.noHead {
				http.Error(w, "no HEAD here", http.StatusMethodNotAllowed)
				return
			}
		} else {
			o.gets++
		}
		if im := r.Header.Get("If-Match"); im != "" {
			o.ifMatch = append(o.ifMatch, im)
		}
		if o.etag != "" {
			w.Header().Set("ETag", o.etag)
		}
		cw := &countingWriter{ResponseWriter: w}
		http.ServeContent(cw, r, "o.bin", o.modTime, strings.NewReader(string(o.body)))
		o.sent += cw.n
	}))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *objectServer) swap(body []byte, etag string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.body, o.etag = body, etag
	o.modTime = o.modTime.Add(time.Hour)
}

func (o *objectServer) counts() (gets, heads int, sent int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.gets, o.heads, o.sent
}

type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)*7 + seed
	}
	return b
}

// Stat is one HEAD: no window downloaded, and the size, Last-Modified and ETag
// the server declares come back on the FileInfo.
func TestStatIsOneHEAD(t *testing.T) {
	o := newObjectServer(t, pattern(1<<20, 1), `"v1"`)
	fs := New()
	fi, err := fs.Port().DoStat(o.srv.URL + "/o.bin")
	if err != nil {
		t.Fatal(err)
	}
	gets, heads, sent := o.counts()
	if gets != 0 || heads != 1 || sent != 0 || fs.BytesFetched() != 0 {
		t.Fatalf("Stat made %d GET, %d HEAD, sent %d bytes (fetched %d); want one HEAD and nothing transferred", gets, heads, sent, fs.BytesFetched())
	}
	if fi.Size() != 1<<20 {
		t.Errorf("Size = %d", fi.Size())
	}
	if !fi.ModTime().Equal(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("ModTime = %v, want the server's Last-Modified", fi.ModTime())
	}
	id, ok := fi.(mkv.FileIdentity)
	if !ok || id.Identity() != `"v1"` {
		t.Errorf("Identity = %v %v, want the ETag", ok, id)
	}
}

// A server without HEAD still stats: one ranged GET of a single byte.
func TestStatFallsBackToOneByteGET(t *testing.T) {
	o := newObjectServer(t, pattern(1<<20, 1), `"v1"`)
	o.noHead = true
	fs := New()
	fi, err := fs.Port().DoStat(o.srv.URL + "/o.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, sent := o.counts(); sent > 1 || fi.Size() != 1<<20 {
		t.Fatalf("fallback sent %d bytes, size %d", sent, fi.Size())
	}
}

// A reader never mixes two versions: once it has seen one ETag, a window of
// another version is refused with mkv.ErrSourceChanged, with If-Match on the wire.
func TestReaderRefusesAnotherVersion(t *testing.T) {
	o := newObjectServer(t, pattern(4096, 1), `"v1"`)
	f, err := New(Options{WindowSize: 1024}).Port().DoOpen(o.srv.URL + "/o.bin")
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 1024)
	if _, err := io.ReadFull(f, first); err != nil {
		t.Fatal(err)
	}
	o.swap(pattern(4096, 2), `"v2"`)
	second := make([]byte, 1024)
	_, err = io.ReadFull(f, second)
	if !errors.Is(err, mkv.ErrSourceChanged) {
		t.Fatalf("read across a swapped object: err = %v, want mkv.ErrSourceChanged", err)
	}
	o.mu.Lock()
	im := o.ifMatch
	o.mu.Unlock()
	if len(im) == 0 || im[len(im)-1] != `"v1"` {
		t.Errorf("If-Match sent: %v, want the first version's ETag", im)
	}
}

// Pin binds a fresh reader to the version a plan was built on: the first read
// of another version fails, nothing of it is returned.
func TestPinRefusesOtherVersionOnFirstRead(t *testing.T) {
	o := newObjectServer(t, pattern(4096, 2), `"v2"`)
	f, err := New(Options{WindowSize: 1024}).Port().DoOpen(o.srv.URL + "/o.bin")
	if err != nil {
		t.Fatal(err)
	}
	f.(mkv.PinnedReader).Pin(`"v1"`)
	if _, err := f.Read(make([]byte, 10)); !errors.Is(err, mkv.ErrSourceChanged) {
		t.Fatalf("pinned read of another version: err = %v", err)
	}
	g, _ := New(Options{WindowSize: 1024}).Port().DoOpen(o.srv.URL + "/o.bin")
	g.(mkv.PinnedReader).Pin(`"v2"`)
	if _, err := io.ReadFull(g, make([]byte, 2048)); err != nil {
		t.Fatalf("pinned read of the right version: %v", err)
	}
}

// A 206 whose Content-Range does not start where the request asked is refused.
func TestFetchRejectsMisplacedContentRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-9/100")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("0123456789"))
	}))
	defer srv.Close()
	f, err := New(Options{WindowSize: 10}).Port().DoOpen(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(50, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(make([]byte, 5)); err == nil {
		t.Fatal("a window served from the wrong offset was accepted")
	}
}
