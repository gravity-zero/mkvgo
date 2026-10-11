package mp4

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/httpfs"
)

// remoteSource serves a local fixture over HTTP with an ETag, counting requests.
type remoteSource struct {
	mu     sync.Mutex
	path   string
	etag   string
	gets   int
	heads  int
	zero   int // GET requests of a window at offset 0: what a Stat through a ranged GET costs
	ranges []string
	srv    *httptest.Server
}

func newRemoteSource(t *testing.T, path string) *remoteSource {
	t.Helper()
	r := &remoteSource{path: path, etag: `"v1"`}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		if req.Method == http.MethodHead {
			r.heads++
		} else {
			r.gets++
			r.ranges = append(r.ranges, req.Header.Get("Range"))
			if strings.HasPrefix(req.Header.Get("Range"), "bytes=0-") {
				r.zero++
			}
		}
		path, etag := r.path, r.etag
		r.mu.Unlock()
		f, err := os.Open(path)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer f.Close()
		fi, _ := f.Stat()
		w.Header().Set("ETag", etag)
		http.ServeContent(w, req, "s.mkv", fi.ModTime(), f)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *remoteSource) counts() (gets, heads, zero int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gets, r.heads, r.zero
}

// A plan over httpfs issues no Stat per segment: its reads are pinned to the
// version it was built on, and a replaced object is refused, never mixed in.
func TestRemotePlanPinsItsSource(t *testing.T) {
	src := openGOPSource(t, 6, false, true)
	rs := newRemoteSource(t, src)
	ctx := context.Background()
	fs := httpfs.New(httpfs.Options{WindowSize: 64 << 10}).Port()
	plan, err := PlanHLS(ctx, rs.srv.URL+"/s.mkv", Options{SegmentMs: openGOPMs, FS: fs, StreamFromFS: true})
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := plan.Resource(ctx, "seg00002.m4s")
	if err != nil {
		t.Fatal(err)
	}
	g0, h0, z0 := rs.counts()
	for n := 1; n <= 3; n++ {
		name := "seg0000" + string(rune('0'+n)) + ".m4s"
		h, err := plan.Open(ctx, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var out strings.Builder
		if _, err := h.WriteTo(ctx, &out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	g1, h1, z1 := rs.counts()
	if h1 != h0 || z1 != z0 {
		t.Errorf("serving 3 segments made %d HEAD and %d GET at offset 0: the reads are pinned, no stat is needed", h1-h0, z1-z0)
	}
	t.Logf("3 segments: %d GET, %d HEAD, ranges %v", g1-g0, h1-h0, rs.ranges)
	// The object is replaced by one of the same size and another payload: every read of it must fail.
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)*6/10] ^= 0xFF
	other := src + ".other"
	if err := os.WriteFile(other, data, 0o644); err != nil {
		t.Fatal(err)
	}
	rs.mu.Lock()
	rs.path, rs.etag = other, `"v2"`
	rs.mu.Unlock()
	h, err := plan.Open(ctx, "seg00002.m4s")
	if err == nil {
		var out strings.Builder
		_, err = h.WriteTo(ctx, &out)
		if err == nil && out.String() == string(want) {
			t.Fatal("the old window table served the old bytes from the new object")
		}
	}
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("after the replacement: err = %v, want ErrSourceChanged", err)
	}
	if _, _, err := plan.Resource(ctx, "seg00005.m4s"); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("Resource after the replacement: err = %v, want ErrSourceChanged", err)
	}
}

// openSource verifies the file it actually opened, so a replacement between
// the plan's check and its read is caught on the open file itself.
func TestOpenSourceVerifiesTheOpenFile(t *testing.T) {
	src := openGOPSource(t, 3, false, true)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	f, err := plan.openSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	data, _ := os.ReadFile(src)
	fi, _ := os.Stat(src)
	twin := src + ".twin"
	if err := os.WriteFile(twin, data, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(twin, fi.ModTime(), fi.ModTime())
	if err := os.Rename(twin, src); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.openSource(context.Background()); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("openSource over the replaced file: err = %v", err)
	}
}

// The streamed ETag and SourceID follow the file's identity: a file renamed
// over the source with the same size and mtime gives another one from a new
// plan; the same file gives the same.
func TestSourceIDAndETagFollowIdentity(t *testing.T) {
	src := openGOPSource(t, 3, false, true)
	ctx := context.Background()
	a, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	ha, _ := a.Open(ctx, "seg00001.m4s")
	b, _ := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
	hb, _ := b.Open(ctx, "seg00001.m4s")
	if a.SourceID() == "" || a.SourceID() != b.SourceID() || ha.ETag() != hb.ETag() {
		t.Fatalf("same file: SourceID %q vs %q, ETag %s vs %s", a.SourceID(), b.SourceID(), ha.ETag(), hb.ETag())
	}
	data, _ := os.ReadFile(src)
	fi, _ := os.Stat(src)
	twin := src + ".twin"
	if err := os.WriteFile(twin, data, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(twin, fi.ModTime(), fi.ModTime())
	if err := os.Rename(twin, src); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Stat(src); again.Size() != fi.Size() || !again.ModTime().Equal(fi.ModTime()) {
		t.Skip("the filesystem did not keep the stamp")
	}
	if runtime.GOOS == "windows" {
		t.Skip("no file identity on Windows: a same-stamp replacement keeps the SourceID (documented)")
	}
	c, _ := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
	hc, _ := c.Open(ctx, "seg00001.m4s")
	if c.SourceID() == a.SourceID() || hc.ETag() == ha.ETag() {
		t.Errorf("replaced file with the same stamp: SourceID %q, ETag %s unchanged", c.SourceID(), hc.ETag())
	}
	_ = time.Second
}

// Segments served concurrently while the object is swapped are each either
// the plan's exact bytes or ErrSourceChanged: no result mixes two versions.
func TestRemotePlanConcurrentSwapNeverMixes(t *testing.T) {
	src := openGOPSource(t, 12, false, true)
	rs := newRemoteSource(t, src)
	ctx := context.Background()
	fs := httpfs.New(httpfs.Options{WindowSize: 16 << 10}).Port()
	plan, err := PlanHLS(ctx, rs.srv.URL+"/s.mkv", Options{SegmentMs: openGOPMs, FS: fs, StreamFromFS: true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{}
	for n := 1; n <= 12; n++ {
		for _, name := range []string{fmt.Sprintf("seg%05d.m4s", n), fmt.Sprintf("seg_a1_%05d.m4s", n)} {
			b, _, err := plan.Resource(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			want[name] = b
		}
	}
	data, _ := os.ReadFile(src)
	for i := len(data) / 3; i < len(data); i += 997 {
		data[i] ^= 0x5A
	}
	other := src + ".other"
	if err := os.WriteFile(other, data, 0o644); err != nil {
		t.Fatal(err)
	}
	fresh, err := PlanHLS(ctx, rs.srv.URL+"/s.mkv", Options{SegmentMs: openGOPMs, FS: fs, StreamFromFS: true})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var served, refused, mixed int
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for round := 0; round < 6; round++ {
				n := (g+round)%12 + 1
				name := fmt.Sprintf("seg%05d.m4s", n)
				if round%2 == 1 {
					name = fmt.Sprintf("seg_a1_%05d.m4s", n)
				}
				var out strings.Builder
				h, err := fresh.Open(ctx, name)
				if err == nil {
					_, err = h.WriteTo(ctx, &out)
				}
				mu.Lock()
				switch {
				case err == nil && out.String() == string(want[name]):
					served++
				case errors.Is(err, ErrSourceChanged):
					refused++
				default:
					mixed++
					t.Errorf("%s: err %v, %d bytes (want %d)", name, err, out.Len(), len(want[name]))
				}
				mu.Unlock()
				if g == 0 && round == 1 {
					rs.mu.Lock()
					rs.path, rs.etag = other, `"v2"`
					rs.mu.Unlock()
				}
				time.Sleep(time.Millisecond)
			}
		}(g)
	}
	wg.Wait()
	t.Logf("served %d, refused %d, mixed %d", served, refused, mixed)
	if refused == 0 {
		t.Errorf("the swap was never noticed")
	}
}
