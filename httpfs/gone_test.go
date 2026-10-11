package httpfs

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// An object that shrank under a reader answers 416 to a range it had: another version, not an end of file.
func TestShrunkObjectIsSourceChanged(t *testing.T) {
	var calls atomic.Int32
	url := rawRange(t, func(w http.ResponseWriter, a, b int64) {
		if calls.Add(1) > 1 {
			w.Header().Set("Content-Range", "bytes */50")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/1000", a, b))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(make([]byte, b-a+1))
	})
	f, _ := New(Options{WindowSize: 100}).Port().DoOpen(url)
	if _, err := io.ReadFull(f, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	f.Seek(500, io.SeekStart)
	if _, err := f.Read(make([]byte, 10)); !errors.Is(err, mkv.ErrSourceChanged) {
		t.Fatalf("416 inside the known size: err = %v, want mkv.ErrSourceChanged", err)
	}
}

// An object gone (404, 410) under a reader bound to a version is a changed
// source; a fresh reader's 404 stays a plain not-found.
func TestGoneObject(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusGone} {
		var calls atomic.Int32
		url := rawRange(t, func(w http.ResponseWriter, a, b int64) {
			if calls.Add(1) > 1 {
				w.WriteHeader(code)
				return
			}
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/1000", a, b))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(make([]byte, b-a+1))
		})
		f, _ := New(Options{WindowSize: 100}).Port().DoOpen(url)
		if _, err := io.ReadFull(f, make([]byte, 10)); err != nil {
			t.Fatal(err)
		}
		f.Seek(500, io.SeekStart)
		if _, err := f.Read(make([]byte, 10)); !errors.Is(err, mkv.ErrSourceChanged) {
			t.Errorf("%d after a first window: err = %v, want mkv.ErrSourceChanged", code, err)
		}
		g, _ := New(Options{WindowSize: 100}).Port().DoOpen(url)
		g.(mkv.PinnedReader).Pin(`"v1"`)
		if _, err := g.Read(make([]byte, 10)); !errors.Is(err, mkv.ErrSourceChanged) {
			t.Errorf("%d on a pinned reader: err = %v, want mkv.ErrSourceChanged", code, err)
		}
		h, _ := New(Options{WindowSize: 100}).Port().DoOpen(url)
		if _, err := h.Read(make([]byte, 10)); err == nil || errors.Is(err, mkv.ErrSourceChanged) {
			t.Errorf("%d on a fresh reader: err = %v, want a plain not-found", code, err)
		}
	}
}
