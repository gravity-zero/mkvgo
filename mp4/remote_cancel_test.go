package mp4

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/httpfs"
)

// A walk whose every client has gone releases the HTTP request it is blocked
// on: the server sees the request end and the walk slot comes back.
func TestCancelledWalkReleasesBlockedRequest(t *testing.T) {
	src := openGOPSource(t, 6, false, true)
	var hang atomic.Bool
	released := make(chan struct{}, 16)
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang.Load() && r.Method == http.MethodGet {
			select {
			case <-r.Context().Done():
				released <- struct{}{}
			case <-stop:
			}
			return
		}
		f, err := os.Open(src)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer f.Close()
		fi, _ := f.Stat()
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "s.mkv", fi.ModTime(), f)
	}))
	defer func() { close(stop); srv.Close() }()
	ctx := context.Background()
	fs := httpfs.New(httpfs.Options{WindowSize: 16 << 10, RequestTimeout: -1}).Port()
	plan, err := PlanHLS(ctx, srv.URL+"/s.mkv", Options{SegmentMs: openGOPMs, FS: fs, StreamFromFS: true})
	if err != nil {
		t.Fatal(err)
	}
	hang.Store(true)
	for _, name := range []string{"seg00004.m4s", "seg_a1_00005.m4s"} {
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			if name == "seg00004.m4s" {
				_, err := plan.Open(cctx, name)
				done <- err
				return
			}
			_, _, err := plan.Resource(cctx, name)
			done <- err
		}()
		time.Sleep(100 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s: err = %v, want context.Canceled", name, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: still blocked 3 s after its client left", name)
		}
		select {
		case <-released:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: the server never saw the request end", name)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for WalksInFlight() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := WalksInFlight(); n != 0 {
		t.Fatalf("%d walks still in flight", n)
	}
}
