package httpfs

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

// TestReadAtAcrossARangeCap: a server that serves at most 20 bytes per 206, short of the object's end, is asked
// again for the rest; ReadAt said io.EOF there, as if the object ended.
func TestReadAtAcrossARangeCap(t *testing.T) {
	body := pattern(1000, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"v1"`)
		var a, b int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(body)
			}
			return
		}
		b = min(b, a+19, len(body)-1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(b-a+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[a : b+1])
	}))
	defer srv.Close()
	f, err := New().Port().DoOpen(srv.URL + "/o.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 100)
	if n, err := f.(io.ReaderAt).ReadAt(buf, 500); n != 100 || err != nil || !bytes.Equal(buf, body[500:600]) {
		t.Errorf("ReadAt of 100 bytes through a 20-byte cap = %d, %v", n, err)
	}
	if n, err := f.(io.ReaderAt).ReadAt(buf, 950); n != 50 || err != io.EOF {
		t.Errorf("ReadAt across the end = %d, %v; want 50, io.EOF", n, err)
	}
}

// openBodies counts the responses a client holds open, and the most it held open at once when it sent a request.
type openBodies struct {
	open, peak atomic.Int32
}

func (o *openBodies) RoundTrip(req *http.Request) (*http.Response, error) {
	if n := o.open.Load(); n > o.peak.Load() {
		o.peak.Store(n)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	o.open.Add(1)
	resp.Body = &countedBody{ReadCloser: resp.Body, o: o}
	return resp, nil
}

type countedBody struct {
	io.ReadCloser
	o      *openBodies
	closed bool
}

func (b *countedBody) Close() error {
	if !b.closed {
		b.closed = true
		b.o.open.Add(-1)
	}
	return b.ReadCloser.Close()
}

// cappedServer serves at most limit bytes per 206 and counts the ranged requests.
func cappedServer(body []byte, limit int, requests *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"v1"`)
		var a, b int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(body)
			}
			return
		}
		requests.Add(1)
		b = min(b, a+limit-1, len(body)-1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(b-a+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[a : b+1])
	}))
}

// TestReadAtShortResponsesBounded: a server that serves one byte per 206 is asked again at most 8 times, each response
// closed before the next request; ReadAt recursed once per byte and held every response open until the last.
func TestReadAtShortResponsesBounded(t *testing.T) {
	body := pattern(1000, 5)
	var requests atomic.Int32
	srv := cappedServer(body, 1, &requests)
	defer srv.Close()
	bodies := &openBodies{}
	f, err := New(Options{Client: &http.Client{Transport: bodies}}).Port().DoOpen(srv.URL + "/o.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	requests.Store(0)
	n, err := f.(io.ReaderAt).ReadAt(make([]byte, 100), 100)
	if err == nil || err == io.EOF {
		t.Errorf("ReadAt of 100 bytes through a 1-byte cap = %d, %v; want an error", n, err)
	}
	if got := requests.Load(); got > 9 {
		t.Errorf("ReadAt sent %d requests; want at most 9", got)
	}
	if p := bodies.peak.Load(); p > 0 {
		t.Errorf("a request went out while %d responses were still open", p)
	}
}

// TestReadAtShortResponsesClosedInTurn: through a 10-byte cap, 80 bytes take 8 responses and each one is closed before
// the next request.
func TestReadAtShortResponsesClosedInTurn(t *testing.T) {
	body := pattern(1000, 7)
	var requests atomic.Int32
	srv := cappedServer(body, 10, &requests)
	defer srv.Close()
	bodies := &openBodies{}
	f, err := New(Options{Client: &http.Client{Transport: bodies}}).Port().DoOpen(srv.URL + "/o.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 80)
	if n, err := f.(io.ReaderAt).ReadAt(buf, 300); n != 80 || err != nil || !bytes.Equal(buf, body[300:380]) {
		t.Errorf("ReadAt of 80 bytes through a 10-byte cap = %d, %v", n, err)
	}
	if p := bodies.peak.Load(); p > 0 {
		t.Errorf("a request went out while %d responses were still open", p)
	}
}
