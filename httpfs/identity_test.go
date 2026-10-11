package httpfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

// Under a ShareReads scope a reader's Stat is no HEAD: it asks for both ends
// at once, takes the size and version from them, and the scope's readers
// then read both ends for no request.
func TestSharedStatFetchesBothEnds(t *testing.T) {
	body := pattern(4<<20, 3)
	o := newObjectServer(t, body, `"v1"`)
	fs := New(Options{WindowSize: 64 << 10})
	ctx := mkv.ShareReads(context.Background())
	f, err := fs.Port().DoOpenContext(ctx, o.srv.URL+"/o.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, err := f.(interface{ Stat() (os.FileInfo, error) }).Stat()
	if err != nil {
		t.Fatal(err)
	}
	if gets, heads, _ := o.counts(); gets != 2 || heads != 0 {
		t.Fatalf("Stat made %d GET, %d HEAD; want the two ends, no HEAD", gets, heads)
	}
	if id, _ := fi.(mkv.FileIdentity); fi.Size() != int64(len(body)) || id == nil || id.Identity() != `"v1"` {
		t.Fatalf("Stat = size %d identity %v", fi.Size(), id)
	}
	g, err := fs.Port().DoOpenContext(ctx, o.srv.URL+"/o.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	g.(mkv.PinnedReader).Pin(`"v1"`)
	for _, off := range []int64{0, 1000, int64(len(body)) - 1000} {
		buf := make([]byte, 500)
		if _, err := g.Seek(off, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(g, buf); err != nil || !bytes.Equal(buf, body[off:off+500]) {
			t.Fatalf("read at %d: %v", off, err)
		}
	}
	if gets, _, _ := o.counts(); gets != 2 {
		t.Errorf("reads of both ends made %d more requests, want none", gets-2)
	}
}

// ReadAt is one request of exactly the bytes asked, of the pinned version.
func TestReadAtIsExactAndPinned(t *testing.T) {
	body := pattern(1<<20, 5)
	o := newObjectServer(t, body, `"v1"`)
	f, err := New().Port().DoOpen(o.srv.URL + "/o.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.(mkv.PinnedReader).Pin(`"v1"`)
	buf := make([]byte, 32)
	if n, err := f.(io.ReaderAt).ReadAt(buf, 500000); n != 32 || err != nil || !bytes.Equal(buf, body[500000:500032]) {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	if _, _, sent := o.counts(); sent != 32 {
		t.Errorf("ReadAt of 32 bytes transferred %d", sent)
	}
	if n, err := f.(io.ReaderAt).ReadAt(buf, int64(len(body))-10); n != 10 || err != io.EOF {
		t.Errorf("ReadAt across the end = %d, %v; want 10, io.EOF", n, err)
	}
	o.swap(pattern(1<<20, 6), `"v2"`)
	if _, err := f.(io.ReaderAt).ReadAt(buf, 0); !errors.Is(err, mkv.ErrSourceChanged) {
		t.Errorf("ReadAt of a replaced object: %v, want ErrSourceChanged", err)
	}
}

// The two ends a shared Stat fetches must be of one version: an object
// replaced between the two responses (another ETag, or another total size) is
// ErrSourceChanged, and neither end is kept for the scope's readers.
func TestSharedStatRefusesTwoVersions(t *testing.T) {
	old, repl := pattern(2<<20, 1), pattern(3<<20, 2)
	for _, c := range []struct {
		name       string
		tail       []byte
		etag, tEtg string
	}{
		{"another ETag", old, `"v1"`, `"v2"`},
		{"another size", repl, `"v1"`, `"v1"`},
		{"another Last-Modified, no ETag", old, "", ""},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, etag, mod := old, c.etag, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
			if strings.HasPrefix(r.Header.Get("Range"), "bytes=-") { // the suffix is served by the replacement
				body, etag, mod = c.tail, c.tEtg, mod.Add(time.Hour)
			}
			if etag != "" {
				w.Header().Set("ETag", etag)
			}
			http.ServeContent(w, r, "o.bin", mod, strings.NewReader(string(body)))
		}))
		share := &mkv.ReadShare{}
		fs := New(Options{WindowSize: 64 << 10})
		f, err := fs.Port().DoOpenContext(mkv.WithReadShare(context.Background(), share), srv.URL+"/o.bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.(interface{ Stat() (os.FileInfo, error) }).Stat(); !errors.Is(err, mkv.ErrSourceChanged) {
			t.Errorf("%s: Stat = %v, want ErrSourceChanged", c.name, err)
		}
		if kept, ok := share.LoadOrStore(fs, &recentReads{}).(*recentReads); !ok || len(kept.recent) != 0 {
			t.Errorf("%s: %d responses kept for the scope after a refusal", c.name, len(kept.recent))
		}
		f.Close()
		srv.Close()
	}
}
