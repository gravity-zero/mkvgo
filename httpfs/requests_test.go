package httpfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func ramp(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

// A sequential read doubles each request, up to 32 windows: a megabyte read
// through a 16 KiB window is 7 requests, not 64, and every byte arrives once.
func TestSequentialReadGrowsTheRequest(t *testing.T) {
	body := ramp(1 << 20)
	var ranges []string
	srv := rangeServer(t, body, &ranges)
	defer srv.Close()
	fs := New(Options{WindowSize: 16 << 10})
	f, err := fs.Port().DoOpen(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read %d bytes, err %v", len(got), err)
	}
	if len(ranges) > 7 {
		t.Errorf("%d requests for a sequential megabyte: %v", len(ranges), ranges)
	}
	if n := fs.BytesFetched(); n != int64(len(body)) {
		t.Errorf("fetched %d bytes for %d", n, len(body))
	}
	// A read that jumps away starts over from one window.
	ranges = nil
	if _, err := f.Seek(100, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 || ranges[0] != "bytes=100-16483" {
		t.Errorf("a jump asked %v, want one window", ranges)
	}
}

// A hinted range is fetched with one request, whatever the window.
func TestHintRangeIsOneRequest(t *testing.T) {
	body := ramp(1 << 20)
	var ranges []string
	srv := rangeServer(t, body, &ranges)
	defer srv.Close()
	f, err := New(Options{WindowSize: 4 << 10}).Port().DoOpen(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	f.(mkv.RangeHinter).HintRange(100<<10, 900<<10)
	if _, err := f.Seek(100<<10, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 800<<10)
	if _, err := io.ReadFull(f, got); err != nil || !bytes.Equal(got, body[100<<10:900<<10]) {
		t.Fatalf("hinted read: err %v", err)
	}
	if len(ranges) != 1 || ranges[0] != "bytes=102400-921599" {
		t.Errorf("hinted read asked %v, want one request for the range", ranges)
	}
	// A hint to the end of an object of unknown size asks for the rest of it.
	g, _ := New().Port().DoOpen(srv.URL + "/x")
	g.(mkv.RangeHinter).HintRange(1000, 1<<62)
	g.Seek(1000, io.SeekStart)
	ranges = nil
	rest, err := io.ReadAll(g)
	if err != nil || !bytes.Equal(rest, body[1000:]) || len(ranges) != 1 || ranges[0] != "bytes=1000-" {
		t.Errorf("open hint: %d bytes, err %v, ranges %v", len(rest), err, ranges)
	}
}

// Readers opened under one mkv.ShareReads scope and pinned to one strong ETag
// read again what another fetched, for no request; outside a scope every read
// asks the server, which is how a replaced object is noticed.
func TestShareReadsScope(t *testing.T) {
	body := ramp(64 << 10)
	o := newObjectServer(t, body, `"v1"`)
	fs := New(Options{WindowSize: 16 << 10}).Port()
	url := o.srv.URL + "/o.bin"
	read := func(ctx context.Context, off int64) []byte {
		f, err := fs.DoOpenContext(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		f.(mkv.PinnedReader).Pin(`"v1"`)
		f.Seek(off, io.SeekStart)
		b := make([]byte, 1000)
		if _, err := io.ReadFull(f, b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	shared := mkv.ShareReads(context.Background())
	read(shared, 0)
	if !bytes.Equal(read(shared, 5000), body[5000:6000]) {
		t.Fatal("a shared reread served other bytes")
	}
	if gets, _, _ := o.counts(); gets != 1 {
		t.Errorf("two readers of one scope made %d requests, want 1", gets)
	}
	o.swap(append([]byte{1}, body[1:]...), `"v2"`)
	if f, _ := fs.DoOpen(url); true {
		f.(mkv.PinnedReader).Pin(`"v1"`)
		if _, err := f.Read(make([]byte, 10)); err == nil {
			t.Error("outside a scope a reader pinned to a replaced version read it")
		}
	}
}

// A response that breaks midway is resumed from where it stopped, for the same version.
func TestBrokenResponseResumes(t *testing.T) {
	body := ramp(256 << 10)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spec := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
		x, y, _ := strings.Cut(spec, "-")
		a, _ := strconv.ParseInt(x, 10, 64)
		b, _ := strconv.ParseInt(y, 10, 64)
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", "bytes "+x+"-"+y+"/"+strconv.Itoa(len(body)))
		w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		if calls.Add(1) == 1 { // the first response drops its connection halfway
			w.Write(body[a : a+(b-a+1)/2])
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.Write(body[a : b+1])
	}))
	defer srv.Close()
	f, _ := New(Options{WindowSize: 256 << 10}).Port().DoOpen(srv.URL + "/x")
	got, err := io.ReadAll(f)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read %d of %d bytes, err %v", len(got), len(body), err)
	}
	if calls.Load() != 2 {
		t.Errorf("%d requests, want the broken one and its resumption", calls.Load())
	}
}

// connTrace counts the connections a client's requests got: new or reused.
type connTrace struct{ fresh, reused atomic.Int32 }

func (c *connTrace) RoundTrip(r *http.Request) (*http.Response, error) {
	tr := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
		if i.Reused {
			c.reused.Add(1)
		} else {
			c.fresh.Add(1)
		}
	}}
	return http.DefaultTransport.RoundTrip(r.WithContext(httptrace.WithClientTrace(r.Context(), tr)))
}

// A response left with up to drainMax unread is read out, so the next request
// reuses its connection instead of paying a new handshake; a larger rest is
// not read out (closing it is the transport's call).
func TestAbandonedResponseKeepsItsConnection(t *testing.T) {
	body := ramp(8 << 20)
	srv := rangeServer(t, body, nil)
	defer srv.Close()
	for _, c := range []struct {
		name  string
		hint  int64
		drain bool
	}{{"rest within drainMax", 1 << 20, true}, {"rest past drainMax", 4 << 20, false}} {
		t.Run(c.name, func(t *testing.T) {
			ct := &connTrace{}
			fs := New(Options{Client: &http.Client{Transport: ct}})
			f, _ := fs.Port().DoOpen(srv.URL + "/x")
			f.(mkv.RangeHinter).HintRange(0, c.hint)
			if _, err := io.ReadFull(f, make([]byte, 1000)); err != nil {
				t.Fatal(err)
			}
			f.Seek(6<<20, io.SeekStart) // away from the response: it is abandoned
			if _, err := io.ReadFull(f, make([]byte, 1000)); err != nil {
				t.Fatal(err)
			}
			first := fs.BytesFetched() - 1000
			f.Close()
			if c.drain {
				if first != c.hint || ct.fresh.Load() != 1 {
					t.Errorf("read %d of the first response, %d new connections: want it read out and its connection reused", first, ct.fresh.Load())
				}
			} else if first >= c.hint {
				t.Errorf("read out %d bytes of a response abandoned with %d unread", first, c.hint-1000)
			}
		})
	}
}

// A response that breaks is resumed for the version read so far: when the
// object is replaced between the break and the resumption the read ends with
// mkv.ErrSourceChanged, whether the server honours If-Match (412) or not (the
// resumption carries another ETag), and no byte of the new version is handed
// out after the old ones. Without any validator the response is not resumed.
func TestResumptionIsPinnedToTheVersion(t *testing.T) {
	v1, v2 := ramp(256<<10), append([]byte{0xEE}, ramp(256 << 10)[1:]...)
	for i := range v2 {
		v2[i] ^= 0x55
	}
	for _, c := range []struct {
		name           string
		etags, ifMatch bool
		wantChanged    bool
	}{
		{"server honours If-Match", true, true, true},
		{"server ignores If-Match", true, false, true},
		{"no validator", false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			var sawIfMatch atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				body, etag := v1, `"v1"`
				if n > 1 { // replaced between the break and the resumption
					body, etag = v2, `"v2"`
					sawIfMatch.Store(r.Header.Get("If-Match"))
				}
				if c.ifMatch && r.Header.Get("If-Match") != "" && r.Header.Get("If-Match") != etag {
					w.WriteHeader(http.StatusPreconditionFailed)
					return
				}
				spec := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
				x, y, _ := strings.Cut(spec, "-")
				a, _ := strconv.ParseInt(x, 10, 64)
				b, _ := strconv.ParseInt(y, 10, 64)
				if c.etags {
					w.Header().Set("ETag", etag)
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(body)))
				w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
				w.WriteHeader(http.StatusPartialContent)
				if n == 1 { // the first response drops its connection halfway
					w.Write(body[a : a+(b-a+1)/2])
					w.(http.Flusher).Flush()
					conn, _, _ := w.(http.Hijacker).Hijack()
					conn.Close()
					return
				}
				w.Write(body[a : b+1])
			}))
			defer srv.Close()
			f, _ := New(Options{WindowSize: 256 << 10}).Port().DoOpen(srv.URL + "/x")
			got, err := io.ReadAll(f)
			if err == nil {
				t.Fatalf("read %d bytes across a replacement without an error", len(got))
			}
			if !bytes.HasPrefix(v1, got) {
				t.Fatalf("the %d bytes handed out are not a prefix of the version first read", len(got))
			}
			if errors.Is(err, mkv.ErrSourceChanged) != c.wantChanged {
				t.Errorf("err = %v, ErrSourceChanged wanted: %v", err, c.wantChanged)
			}
			if c.etags {
				if im, _ := sawIfMatch.Load().(string); im != `"v1"` {
					t.Errorf("the resumption asked If-Match %q, want the version read so far", im)
				}
			} else if calls.Load() != 1 {
				t.Errorf("%d requests: a response with no validator must not be resumed", calls.Load())
			}
		})
	}
}
