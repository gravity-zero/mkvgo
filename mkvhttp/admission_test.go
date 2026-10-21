package mkvhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// blockingResolver holds every segment request until released; playlists answer at once.
type blockingResolver struct {
	*fakeResolver
	entered chan string
	release chan struct{}
}

func (b *blockingResolver) Resource(ctx context.Context, name string) ([]byte, string, error) {
	if gated(name) {
		b.entered <- name
		<-b.release
	}
	return b.fakeResolver.Resource(ctx, name)
}

// One in flight and one waiting: the third segment request is turned away
// with a 503 and Retry-After, a playlist is never held, and the waiting one is
// served once the first ends.
func TestHandlerAdmission(t *testing.T) {
	r := &blockingResolver{fakeResolver: newFakeResolver(), entered: make(chan string, 4), release: make(chan struct{})}
	h := Handler(r, Options{MaxInFlight: 1, MaxWaiting: 1})
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	get := func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/seg00001.m4s", nil))
		codes <- rec.Code
	}
	wg.Add(1)
	go get()
	<-r.entered // the first holds the slot
	wg.Add(1)
	go get()
	waitFor(t, func() bool { return h.(*handler).gate.waiting.Load() == 1 })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/seg00001.m4s", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("over the queue: %d Retry-After=%q Cache-Control=%q, want 503, 1 and no-store", rec.Code, rec.Header().Get("Retry-After"), rec.Header().Get("Cache-Control"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/master.m3u8", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("playlist while full: %d", rec.Code)
	}

	r.release <- struct{}{} // the first ends
	<-r.entered             // the waiting one takes the slot
	r.release <- struct{}{}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Errorf("admitted request: %d", c)
		}
	}
}

// A waiting request whose client leaves gives its place up.
func TestHandlerAdmissionWaiterLeaves(t *testing.T) {
	r := &blockingResolver{fakeResolver: newFakeResolver(), entered: make(chan string, 1), release: make(chan struct{})}
	h := Handler(r, Options{MaxInFlight: 1, MaxWaiting: 1})
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/seg00001.m4s", nil))
	<-r.entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/seg00001.m4s", nil).WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("waiter whose client left: %d", rec.Code)
	}
	if n := h.(*handler).gate.waiting.Load(); n != 0 {
		t.Fatalf("queue after the waiter left: %d", n)
	}
	r.release <- struct{}{}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not reached")
}
