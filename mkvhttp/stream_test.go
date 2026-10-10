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
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
	"github.com/gravity-zero/mkvgo/mp4"
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
