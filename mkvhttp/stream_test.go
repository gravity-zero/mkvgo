package mkvhttp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
	"github.com/gravity-zero/mkvgo/mp4"
)

// Both plans stream through Handler: they resolve and open resources.
var (
	_ Opener = (*mp4.HLSPlan)(nil)
	_ Opener = (*mp4.ConcatPlan)(nil)
	_ Opener = (*mp4.ABRPlan)(nil)
)

// streamFixture writes 10 s of fake H.264 (keyframe every second) with 20 ms AAC audio.
func streamFixture(t *testing.T) string {
	t.Helper()
	avcc := []byte{0x01, 0x64, 0x00, 0x1F, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x42, 0x00, 0x1F, 0x01, 0x00, 0x04, 0x68, 0xCE, 0x3C, 0x80}
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: []byte{0x12, 0x10}, SampleRate: &sr, Channels: &ch},
	}
	var blks []mkv.Block
	for i := 0; i < 250; i++ {
		frame := make([]byte, 600)
		frame[0], frame[1], frame[2], frame[3], frame[4], frame[5] = 0, 0, 0, 1, 0x65, byte(i)
		blks = append(blks, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 40, Keyframe: i%25 == 0, Data: frame})
		if i%2 == 0 {
			blks = append(blks, mkv.Block{TrackNumber: 2, Timecode: int64(i) * 40, Keyframe: true, Data: []byte{0xAA, byte(i), 1, 2, 3}})
			blks = append(blks, mkv.Block{TrackNumber: 2, Timecode: int64(i)*40 + 20, Keyframe: true, Data: []byte{0xAB, byte(i), 4, 5, 6}})
		}
	}
	path := filepath.Join(t.TempDir(), "s.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}, tracks, 10000); err != nil {
		t.Fatal(err)
	}
	start := 0
	for i := 1; i <= len(blks); i++ {
		if i == len(blks) || (blks[i].TrackNumber == 1 && blks[i].Keyframe) {
			if err := m.WriteClusterWithCues(blks[start].Timecode, 1_000_000, blks[start:i]); err != nil {
				t.Fatal(err)
			}
			start = i
		}
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// A streamed segment comes back whole, with its exact length, honours a Range
// and a HEAD, and a conditional GET on its ETag answers 304.
func TestHandlerStreamsSegments(t *testing.T) {
	src := streamFixture(t)
	plan, err := mp4.PlanHLS(context.Background(), src, mp4.Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(plan))
	defer srv.Close()
	for _, name := range []string{"seg00002.m4s", "seg_a1_00003.m4s", "playlist.m3u8", "init.mp4"} {
		want, _, err := plan.Resource(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(srv.URL + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Equal(got, want) || resp.ContentLength != int64(len(want)) {
			t.Fatalf("%s: status %d, %d bytes (want %d), Content-Length %d", name, resp.StatusCode, len(got), len(want), resp.ContentLength)
		}
		etag := resp.Header.Get("ETag")
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/"+name, nil)
		req.Header.Set("If-None-Match", etag)
		if r2, _ := http.DefaultClient.Do(req); r2.StatusCode != http.StatusNotModified {
			t.Errorf("%s: If-None-Match %s answered %d", name, etag, r2.StatusCode)
		}
	}
	want, _, _ := plan.Resource(context.Background(), "seg00002.m4s")
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/seg00002.m4s", nil)
	req.Header.Set("Range", "bytes=100-299")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(part, want[100:300]) || resp.Header.Get("Content-Range") != "bytes 100-299/"+strconv.Itoa(len(want)) {
		t.Fatalf("range: status %d, %d bytes, Content-Range %q", resp.StatusCode, len(part), resp.Header.Get("Content-Range"))
	}
	head, _ := http.Head(srv.URL + "/seg00002.m4s")
	if head.ContentLength != int64(len(want)) {
		t.Errorf("HEAD Content-Length %d, want %d", head.ContentLength, len(want))
	}
	if st := plan.Stats(); st.StreamedSegments == 0 {
		t.Errorf("nothing streamed: %+v", st)
	}
}

// A HEAD and a revalidation answered 304 serve no segment: the plan counts none and a GET after them still gets
// the bytes the walk kept, without reading the source again.
func TestHandlerHeadAndRevalidationServeNothing(t *testing.T) {
	src := streamFixture(t)
	ctx := context.Background()
	plan, err := mp4.PlanHLS(ctx, src, mp4.Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(plan))
	defer srv.Close()
	get := func(method, name, inm string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+"/"+name, nil)
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	first := get(http.MethodGet, "seg00002.m4s", "")
	// The handler counts what it wrote once the write returns, which can be after the client has read it all.
	for deadline := time.Now().Add(5 * time.Second); plan.Stats().StreamedBytes < first.ContentLength && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
	}
	before := plan.Stats()
	head := get(http.MethodHead, "seg_a1_00002.m4s", "")
	if head.StatusCode != 200 || head.ContentLength <= 0 || head.Header.Get("ETag") == "" {
		t.Fatalf("HEAD: status %d, Content-Length %d, ETag %q", head.StatusCode, head.ContentLength, head.Header.Get("ETag"))
	}
	if r := get(http.MethodGet, "seg00002.m4s", first.Header.Get("ETag")); r.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation answered %d", r.StatusCode)
	}
	if st := plan.Stats(); st.ServedSegments != before.ServedSegments || st.StreamedBytes != before.StreamedBytes || st.KeptSegments != before.KeptSegments {
		t.Errorf("a HEAD and a 304 counted as served:\nbefore %+v\nafter  %+v", before, st)
	}
	read := plan.Stats().SourceReadBytes
	audio := get(http.MethodGet, "seg_a1_00002.m4s", "")
	if audio.ContentLength != head.ContentLength || audio.Header.Get("ETag") != head.Header.Get("ETag") {
		t.Errorf("GET after HEAD: Content-Length %d ETag %q, HEAD said %d %q", audio.ContentLength, audio.Header.Get("ETag"), head.ContentLength, head.Header.Get("ETag"))
	}
	if st := plan.Stats(); st.KeptSegments > before.KeptSegments && st.SourceReadBytes != read {
		t.Errorf("the kept audio was read from the source again (%d bytes)", st.SourceReadBytes-read)
	}
}

// A concat part's segment answers a HEAD and a revalidation from its part's Stat: the part counts nothing served.
func TestHandlerConcatHeadServesNothing(t *testing.T) {
	ctx := context.Background()
	src := streamFixture(t)
	cp, err := mp4.PlanConcat(ctx, []string{src, src}, mp4.Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(cp))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/p1/seg00002.m4s")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	before := cp.Part(1).Stats().ServedSegments
	for _, inm := range []string{"", resp.Header.Get("ETag")} {
		method := http.MethodHead
		if inm != "" {
			method = http.MethodGet
		}
		req, _ := http.NewRequest(method, srv.URL+"/p1/seg00002.m4s", nil)
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if want := map[bool]int{true: http.StatusNotModified, false: http.StatusOK}[inm != ""]; r.StatusCode != want || r.Header.Get("ETag") != resp.Header.Get("ETag") {
			t.Errorf("%s (If-None-Match %q): status %d ETag %q", method, inm, r.StatusCode, r.Header.Get("ETag"))
		}
	}
	if got := cp.Part(1).Stats().ServedSegments; got != before {
		t.Errorf("a HEAD and a 304 counted %d segments served", got-before)
	}
}

// An ABR variant's segment streams through its variant's Open like an HLSPlan's, where the ABR plan was served whole
// from memory, and its HEAD answers from Stat.
func TestHandlerStreamsABRVariants(t *testing.T) {
	ctx := context.Background()
	src := streamFixture(t)
	abr, err := mp4.PlanABR(ctx, []string{src, src}, mp4.Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := abr.Resource(ctx, "v1/seg00002.m4s")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(abr))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/seg00002.m4s")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, want) || !strings.HasPrefix(resp.Header.Get("ETag"), "W/") {
		t.Errorf("v1 segment: %d bytes (want %d), ETag %q, want streamed under a weak ETag", len(got), len(want), resp.Header.Get("ETag"))
	}
	before := abr.Variant(0).Stats().ServedSegments
	head, err := http.Head(srv.URL + "/v1/seg00002.m4s")
	if err != nil {
		t.Fatal(err)
	}
	head.Body.Close()
	if head.ContentLength != int64(len(want)) || abr.Variant(0).Stats().ServedSegments != before {
		t.Errorf("HEAD: Content-Length %d (want %d), %d segments counted served", head.ContentLength, len(want), abr.Variant(0).Stats().ServedSegments-before)
	}
}

// A HEAD or a revalidation of a playlist or an init counts nothing streamed: only a GET hands the bytes out.
func TestHandlerHeadOfAPlaylistStreamsNothing(t *testing.T) {
	ctx := context.Background()
	plan, err := mp4.PlanHLS(ctx, streamFixture(t), mp4.Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(plan))
	defer srv.Close()
	for _, name := range []string{"playlist.m3u8", "init.mp4"} {
		resp, err := http.Get(srv.URL + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		before := plan.Stats().StreamedBytes
		head, err := http.Head(srv.URL + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		head.Body.Close()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/"+name, nil)
		req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if head.ContentLength != resp.ContentLength || head.Header.Get("ETag") != resp.Header.Get("ETag") || r.StatusCode != http.StatusNotModified {
			t.Errorf("%s: HEAD %d %q, revalidation %d; GET said %d %q", name, head.ContentLength, head.Header.Get("ETag"), r.StatusCode, resp.ContentLength, resp.Header.Get("ETag"))
		}
		if got := plan.Stats().StreamedBytes - before; got != 0 {
			t.Errorf("%s: a HEAD and a 304 counted %d bytes streamed", name, got)
		}
	}
}

// A name the plan does not serve answers 404, not the 502 of a source that failed: an unknown name, a segment past
// the last, a subtitle rendition the plan does not have.
func TestHandlerUnknownNamesAre404(t *testing.T) {
	plan, err := mp4.PlanHLS(context.Background(), streamFixture(t), mp4.Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(plan))
	defer srv.Close()
	for _, name := range []string{"nope.m3u8", "seg99999.m4s", "seg_a1_99999.m4s", "sub7.m3u8"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			req, _ := http.NewRequest(method, srv.URL+"/"+name, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s answered %d, want 404", method, name, resp.StatusCode)
			}
		}
	}
}

// failingFile fails every read past a shared byte budget: the source breaking midway.
type failingFile struct {
	f    *os.File
	left *int64
}

func (f *failingFile) Read(p []byte) (int, error) {
	if *f.left <= 0 {
		return 0, errors.New("disk gone")
	}
	if int64(len(p)) > *f.left {
		p = p[:*f.left]
	}
	n, err := f.f.Read(p)
	*f.left -= int64(n)
	return n, err
}
func (f *failingFile) Seek(o int64, w int) (int64, error) { return f.f.Seek(o, w) }
func (f *failingFile) Close() error                       { return f.f.Close() }

// A source that fails while a segment streams cuts the connection: the client
// gets an error, never a short body that looks complete.
func TestHandlerCutsConnectionOnMidStreamError(t *testing.T) {
	src := streamFixture(t)
	left := int64(1 << 40)
	fs := &mkv.FS{Open: func(p string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		return &failingFile{f: f, left: &left}, nil
	}}
	plan, err := mp4.PlanHLS(context.Background(), src, mp4.Options{SegmentMs: 2000, FS: fs, StreamFromFS: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Open(context.Background(), "seg00002.m4s"); err != nil { // the table, before the budget bites
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(plan))
	defer srv.Close()
	left = 2000 // enough for the head and a few samples, not the segment
	resp, err := http.Get(srv.URL + "/seg00002.m4s")
	if err != nil {
		return // the connection may already be gone: also a cut, never a clean body
	}
	body, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr == nil && int64(len(body)) == resp.ContentLength {
		t.Fatalf("client got a complete-looking body of %d bytes after a mid-stream failure", len(body))
	}
	if rerr == nil {
		t.Fatalf("client read %d of %d bytes with no error", len(body), resp.ContentLength)
	}
}

// TestByteRangeOfAnEmptyResource: no Range can be satisfied on an empty resource; a suffix range gave a 206 with
// Content-Range "bytes 0--1/0".
func TestByteRangeOfAnEmptyResource(t *testing.T) {
	for _, h := range []string{"bytes=-5", "bytes=0-", "bytes=0-10"} {
		if _, _, _, ok := byteRange(h, 0); ok {
			t.Errorf("%s on an empty resource was satisfiable", h)
		}
	}
	if _, n, status, ok := byteRange("", 0); !ok || n != 0 || status != http.StatusOK {
		t.Errorf("no Range on an empty resource: %d %d %v", n, status, ok)
	}
}
