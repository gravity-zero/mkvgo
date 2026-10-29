package ops

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/httpfs"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mp4"
)

// TestRoutesByContent_RemoteCostsNoRequest: the content sniff opened a remote source once more than its reader did,
// one ranged GET per call; it hands its response on, so Playability asks what a metadata read alone asks.
func TestRoutesByContent_RemoteCostsNoRequest(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"gop2s_aac.mkv", "bframes_aac.mp4"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "internal", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		count := func(read func(url string, fs *mkv.FS)) int64 {
			var n atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					n.Add(1)
				}
				http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
			}))
			defer srv.Close()
			read(srv.URL+"/"+name, httpfs.New().Port())
			return n.Load()
		}
		alone := count(func(url string, fs *mkv.FS) {
			var err error
			if strings.HasSuffix(name, ".mp4") {
				_, _, err = mp4.OpenMetaWithFS(ctx, url, fs)
			} else {
				_, err = reader.OpenMetaWithFS(ctx, url, fs)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
		routed := count(func(url string, fs *mkv.FS) {
			if _, err := Playability(ctx, url, Target{}, mkv.Options{FS: fs}); err != nil {
				t.Fatal(err)
			}
		})
		if routed > alone { // fewer for an MP4: the handed-on reader knows the size a fresh one asks for
			t.Errorf("%s: Playability made %d GETs, a metadata read alone %d", name, routed, alone)
		}
	}
}

// TestRoutesByContent_ReadErrorSurfaces: a source the sniff could not read went to the Matroska reader, whose own
// read then got the MP4 and said "not Matroska"; the sniff's read error is what the caller gets.
func TestRoutesByContent_ReadErrorSurfaces(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "testdata", "bframes_aac.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	var gets atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && gets.Add(1) == 1 {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		http.ServeContent(w, r, "a.mp4", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	_, err = Playability(context.Background(), srv.URL+"/a.mp4", Target{}, mkv.Options{FS: httpfs.New().Port()})
	if err == nil || errors.Is(err, reader.ErrNotMatroska) || !strings.Contains(err.Error(), "500") {
		t.Errorf("got %v, want the HTTP 500 the first read met", err)
	}
}
