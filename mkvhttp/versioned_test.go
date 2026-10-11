package mkvhttp

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gravity-zero/mkvgo/mp4"
)

// versionedServer is the pattern docs/library.md gives for /hls/<SourceID>/<name>:
// one plan per source, dropped on ErrSourceChanged, and a SourceID that is
// not the current plan's answered 410 before anything is read.
type versionedServer struct {
	src  string
	mu   sync.Mutex
	plan *mp4.HLSPlan
}

func (s *versionedServer) current(ctx context.Context) (*mp4.HLSPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plan == nil {
		p, err := mp4.PlanHLS(ctx, s.src, mp4.Options{SegmentMs: 2000})
		if err != nil {
			return nil, err
		}
		s.plan = p
	}
	return s.plan, nil
}

func (s *versionedServer) drop(p *mp4.HLSPlan) {
	s.mu.Lock()
	if s.plan == p {
		s.plan = nil
	}
	s.mu.Unlock()
}

func (s *versionedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, name, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/hls/"), "/")
	plan, err := s.current(r.Context())
	if err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if !ok || id != plan.SourceID() {
		http.Error(w, "gone: another version of the source", http.StatusGone)
		return
	}
	if _, err := plan.Open(r.Context(), name); errors.Is(err, mp4.ErrSourceChanged) {
		s.drop(plan) // the next request plans the new file, under a new SourceID
		http.Error(w, "gone: the source changed", http.StatusGone)
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + name
	Handler(plan, Options{Immutable: true}).ServeHTTP(w, r2)
}

// An URL that names an old SourceID never serves the new generation's bytes,
// before or after the plan is rebuilt.
func TestVersionedURLNeverServesAnotherGeneration(t *testing.T) {
	src := streamFixture(t)
	other := streamFixture(t)
	data, _ := os.ReadFile(other)
	for i := len(data) / 3; i < len(data); i += 509 { // another payload, same size and layout
		data[i] ^= 0x5A
	}
	s := &versionedServer{src: src}
	srv := httptest.NewServer(s)
	defer srv.Close()
	p1, err := s.current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	old := srv.URL + "/hls/" + p1.SourceID() + "/seg00002.m4s"
	want1, _, _ := p1.Resource(context.Background(), "seg00002.m4s")
	resp := get(t, old, nil)
	if b := body(t, resp); resp.StatusCode != http.StatusOK || !bytes.Equal(b, want1) {
		t.Fatalf("first generation: %d, %d bytes", resp.StatusCode, len(b))
	}
	if err := os.WriteFile(src+".tmp", data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src+".tmp", src); err != nil {
		t.Fatal(err)
	}
	for i, hdr := range []map[string]string{nil, nil, {"Range": "bytes=0-99"}, {"If-None-Match": resp.Header.Get("ETag")}} {
		r := get(t, old, hdr)
		b := body(t, r)
		if r.StatusCode != http.StatusGone {
			t.Fatalf("old URL, request %d after the replacement: status %d, %d bytes", i, r.StatusCode, len(b))
		}
	}
	p2, err := s.current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p2 == p1 || p2.SourceID() == p1.SourceID() {
		t.Fatalf("the plan was not rebuilt under a new SourceID (%s)", p2.SourceID())
	}
	want2, _, _ := p2.Resource(context.Background(), "seg00002.m4s")
	r := get(t, srv.URL+"/hls/"+p2.SourceID()+"/seg00002.m4s", nil)
	if b := body(t, r); r.StatusCode != http.StatusOK || !bytes.Equal(b, want2) || bytes.Equal(b, want1) {
		t.Fatalf("new URL: status %d, new bytes %v", r.StatusCode, bytes.Equal(b, want2))
	}
}
