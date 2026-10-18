package mp4

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gravity-zero/mkvgo/httpfs"
	"github.com/gravity-zero/mkvgo/mkv"
)

type countedWrites struct {
	*os.File
	n *int64
}

func (f countedWrites) Write(p []byte) (int, error) {
	k, err := f.File.Write(p)
	*f.n += int64(k)
	return k, err
}

func fastStartSource(t *testing.T, tag byte) string {
	t.Helper()
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: u8p(2), SampleRate: f64p(48000)},
		{ID: 3, Type: mkv.SubtitleTrack, Codec: "hdmv_pgs"},
	}
	var blocks []genBlock
	for i := int64(0); i < 50; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: i * 40, key: i%10 == 0, data: bytes.Repeat([]byte{'V', tag}, 500+int(i))})
		blocks = append(blocks, genBlock{track: 2, pts: i * 40, key: true, data: bytes.Repeat([]byte{'A', tag}, 40)})
	}
	return buildMKV(t, tracks, blocks)
}

// A fast-start remux writes the output once, in place: no temporary file, no
// byte written twice, and a dropped track reported once over the two passes.
func TestFastStartWritesOnce(t *testing.T) {
	src := fastStartSource(t, 1)
	dst := filepath.Join(t.TempDir(), "fs.mp4")
	var creates []string
	var written int64
	fs := &mkv.FS{Create: func(p string) (mkv.WriteSeekCloser, error) {
		creates = append(creates, p)
		f, err := os.Create(p)
		if err != nil {
			return nil, err
		}
		return countedWrites{f, &written}, nil
	}}
	drops := 0
	if err := RemuxToMP4(context.Background(), src, dst, Options{FS: fs, FastStart: true, OnDrop: func(DroppedTrack) { drops++ }}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(creates) != 1 || creates[0] != dst {
		t.Errorf("files created: %v, want the output alone", creates)
	}
	if written != st.Size() {
		t.Errorf("wrote %d bytes for a %d-byte output", written, st.Size())
	}
	if drops != 1 {
		t.Errorf("the PGS track was reported dropped %d times, want once", drops)
	}
	// Same media, same moov as the default layout, only moved ahead.
	plain := filepath.Join(t.TempDir(), "plain.mp4")
	if err := RemuxToMP4(context.Background(), src, plain, Options{SkipUnsupported: true}); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(plain)
	b, _ := os.ReadFile(dst)
	if len(a) != len(b) {
		t.Errorf("fast start is %d bytes, the default layout %d", len(b), len(a))
	}
}

// A source that changes between the sizing and the write pass is refused, and
// no output is left behind: the moov up front would not describe the media.
func TestFastStartRefusesSourceChangedBetweenPasses(t *testing.T) {
	first, second := fastStartSource(t, 1), fastStartSource(t, 2)
	opens := 0
	fs := &mkv.FS{Open: func(p string) (mkv.ReadSeekCloser, error) {
		opens++
		if opens > 3 { // metadata, the remote probe, the sizing pass, then the write pass reads another file
			return os.Open(second)
		}
		return os.Open(p)
	}}
	// Same sizes, other bytes: only the content hashes tell them apart.
	dst := filepath.Join(t.TempDir(), "fs.mp4")
	err := RemuxToMP4(context.Background(), first, dst, Options{FS: fs, FastStart: true, SkipUnsupported: true, ContentHashes: true})
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("err = %v, want ErrSourceChanged", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("a refused remux left %s behind", dst)
	}
}

// Past the 64-bit offset threshold (lowered here: a real case needs a 4 GiB
// output) every layout writes co64, a larger moov the fast start sizes before
// writing, and the samples resolve the same: the default layout, the one-pass
// fast start of a local source, and the temporary-file fast start of a remote
// one, which downloads the source once.
func TestFastStartCo64(t *testing.T) {
	old := co64Above
	co64Above = 0
	defer func() { co64Above = old }()
	src := fastStartSource(t, 3)
	st, _ := os.Stat(src)
	serve := func(sent *atomic.Int64) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f, err := os.Open(src)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			defer f.Close()
			w.Header().Set("ETag", `"v1"`)
			http.ServeContent(countingResponse{w, sent}, r, "s.mkv", st.ModTime(), f)
		}))
	}
	var sent, plainSent atomic.Int64
	srv := serve(&sent)
	defer srv.Close()

	dir := t.TempDir()
	type run struct {
		name, src string
		o         Options
	}
	runs := []run{
		{"default", src, Options{SkipUnsupported: true}},
		{"fast start, local", src, Options{SkipUnsupported: true, FastStart: true}},
		{"fast start, remote", srv.URL + "/s.mkv", Options{SkipUnsupported: true, FastStart: true, FS: httpfs.Hybrid()}},
	}
	var want [][]byte
	for i, r := range runs {
		dst := filepath.Join(dir, fmt.Sprintf("o%d.mp4", i))
		if err := RemuxToMP4(context.Background(), r.src, dst, r.o); err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		data, _ := os.ReadFile(dst)
		boxes := walkBoxes(t, data, 0)
		moov := mustBox(t, boxes, "moov")
		if !bytes.Contains(moov.payload, []byte("co64")) || bytes.Contains(moov.payload, []byte("stco")) {
			t.Fatalf("%s: chunk offsets are not 64-bit", r.name)
		}
		var video parsedTrack
		for _, pt := range moovTraks(t, boxes) {
			if pt.handler == "vide" {
				video = pt
			}
		}
		got := extractSamples(t, data, video)
		if i == 0 {
			want = got
			continue
		}
		assertSamplesEqual(t, r.name, got, want)
		if _, err := os.Stat(dst + ".mdat.tmp"); !os.IsNotExist(err) {
			t.Errorf("%s: the temporary file is left behind", r.name)
		}
	}
	// The remote fast start downloads what a remote remux without it does: the source once.
	// Each count is read once its server's handlers have all returned (Close waits for them).
	srv.Close()
	fast := sent.Load()
	plainSrv := serve(&plainSent)
	if err := RemuxToMP4(context.Background(), plainSrv.URL+"/s.mkv", filepath.Join(dir, "plain-remote.mp4"), Options{SkipUnsupported: true, FS: httpfs.Hybrid()}); err != nil {
		t.Fatal(err)
	}
	plainSrv.Close()
	if plain := plainSent.Load(); fast != plain {
		t.Errorf("the remote fast start downloaded %d bytes, a remote remux without it %d", fast, plain)
	}
}

type countingResponse struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (w countingResponse) Write(p []byte) (int, error) {
	k, err := w.ResponseWriter.Write(p)
	w.n.Add(int64(k))
	return k, err
}
