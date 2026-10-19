package httpfs

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// TestReadAtAcrossARangeCap: a server that serves at most 10 bytes per 206, short of the object's end, is asked
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
		b = min(b, a+9, len(body)-1)
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
		t.Errorf("ReadAt of 100 bytes through a 10-byte cap = %d, %v", n, err)
	}
	if n, err := f.(io.ReaderAt).ReadAt(buf, 950); n != 50 || err != io.EOF {
		t.Errorf("ReadAt across the end = %d, %v; want 50, io.EOF", n, err)
	}
}
