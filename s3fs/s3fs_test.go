package s3fs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// rangeServer serves data with Range support (206 Partial Content, honouring
// Range: bytes=a-b), mimicking S3's GET behaviour closely enough for httpfs.
func rangeServer(t *testing.T, data []byte, onRequest func(*http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onRequest != nil {
			onRequest(r)
		}
		rng := r.Header.Get("Range")
		start, end := int64(0), int64(len(data))-1
		if rng != "" {
			const prefix = "bytes="
			if !strings.HasPrefix(rng, prefix) {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
			parts := strings.SplitN(strings.TrimPrefix(rng, prefix), "-", 2)
			if v, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
				start = v
			}
			if len(parts) > 1 && parts[1] != "" {
				if v, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
					end = v
				}
			}
		}
		if end >= int64(len(data)) {
			end = int64(len(data)) - 1
		}
		if start > end || start >= int64(len(data)) {
			w.Header().Set("Content-Range", "bytes */"+strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[start : end+1])
	}))
}

// TestFakeEndpoint_HeadersAndURLStyles asserts the request the client actually
// sends carries Authorization/x-amz-date/x-amz-content-sha256=UNSIGNED-PAYLOAD
// and passes Range through, for both path-style and virtual-host-style URLs.
func TestFakeEndpoint_HeadersAndURLStyles(t *testing.T) {
	data := []byte("0123456789ABCDEFGHIJ")

	t.Run("path-style", func(t *testing.T) {
		var gotHost, gotAuth, gotSha, gotDate, gotRange string
		srv := rangeServer(t, data, func(r *http.Request) {
			gotHost = r.Host
			gotAuth = r.Header.Get("Authorization")
			gotSha = r.Header.Get("x-amz-content-sha256")
			gotDate = r.Header.Get("x-amz-date")
			gotRange = r.Header.Get("Range")
			if !strings.HasPrefix(r.URL.Path, "/mybucket/") {
				t.Errorf("path-style request path = %q, want prefix /mybucket/", r.URL.Path)
			}
		})
		defer srv.Close()

		fs := New(Options{
			Region: "us-east-1", Endpoint: srv.URL, PathStyle: true,
			AccessKey: "AKID", SecretKey: "SECRET",
		})
		port := fs.Port()
		f, err := port.DoOpen("s3://mybucket/movie.mkv")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer f.Close()
		buf := make([]byte, 4)
		if _, err := f.Read(buf); err != nil {
			t.Fatalf("Read: %v", err)
		}

		if gotAuth == "" || !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
			t.Errorf("Authorization header missing/malformed: %q", gotAuth)
		}
		if gotSha != "UNSIGNED-PAYLOAD" {
			t.Errorf("x-amz-content-sha256 = %q, want UNSIGNED-PAYLOAD", gotSha)
		}
		if gotDate == "" {
			t.Error("x-amz-date header missing")
		}
		if gotRange == "" {
			t.Error("Range header missing")
		}
		_ = gotHost
	})

	t.Run("virtual-host-style", func(t *testing.T) {
		srv := rangeServer(t, data, nil)
		defer srv.Close()

		// The virtual-host style prefixes the bucket as a subdomain of the
		// endpoint host; against a loopback httptest server that host is not
		// resolvable, so this sub-test only checks the URL construction
		// itself (resolveURL), not a live round trip.
		fs := New(Options{Region: "us-east-1", Endpoint: srv.URL, PathStyle: false})
		u, err := fs.resolveURL("s3://mybucket/dir/movie.mkv")
		if err != nil {
			t.Fatal(err)
		}
		endpointHost := strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://")
		want := "http://mybucket." + endpointHost + "/dir/movie.mkv"
		if u != want {
			t.Errorf("resolveURL = %q, want %q", u, want)
		}
	})

	t.Run("path-style URL construction", func(t *testing.T) {
		fs := New(Options{Region: "us-west-2", PathStyle: true})
		u, err := fs.resolveURL("s3://mybucket/dir/movie.mkv")
		if err != nil {
			t.Fatal(err)
		}
		want := "https://s3.us-west-2.amazonaws.com/mybucket/dir/movie.mkv"
		if u != want {
			t.Errorf("resolveURL = %q, want %q", u, want)
		}
	})

	t.Run("virtual-host default AWS URL construction", func(t *testing.T) {
		fs := New(Options{Region: "us-west-2"})
		u, err := fs.resolveURL("s3://mybucket/dir/movie.mkv")
		if err != nil {
			t.Fatal(err)
		}
		want := "https://mybucket.s3.us-west-2.amazonaws.com/dir/movie.mkv"
		if u != want {
			t.Errorf("resolveURL = %q, want %q", u, want)
		}
	})
}

// buildTinyMKV writes a minimal but structurally real MKV (one video track,
// one cluster) to a temp file and returns its bytes.
func buildTinyMKV(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tiny.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	width, height := uint32(640), uint32(360)
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test"},
	}
	tracks := []mkv.Track{{
		ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng",
		Width: &width, Height: &height, CodecPrivate: []byte{0x01, 0x02, 0x03},
	}}
	if err := mw.WriteMetadata(c, tracks, 1000); err != nil {
		t.Fatal(err)
	}
	blocks := []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: bytes100()}}
	if err := mw.WriteClusterWithCues(0, 1_000_000, blocks); err != nil {
		t.Fatal(err)
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func bytes100() []byte {
	b := make([]byte, 100)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// TestEndToEnd_ServeAndRead serves a small writer-built MKV from an httptest
// server with Range support, then reads it through s3fs.Port() via
// reader.OpenMetaWithFS - metadata should parse correctly and the bytes
// actually fetched should be well under the file size (windowed reads, not a
// full download).
func TestEndToEnd_ServeAndRead(t *testing.T) {
	data := buildTinyMKV(t)
	srv := rangeServer(t, data, nil)
	defer srv.Close()

	fs := New(Options{
		Region: "us-east-1", Endpoint: srv.URL, PathStyle: true,
		AccessKey: "AKID", SecretKey: "SECRET",
		WindowSize: 256, // small window, so metadata-only need not fetch the whole (tiny) file
	})

	c, err := reader.OpenMetaWithFS(context.Background(), "s3://testbucket/tiny.mkv", fs.Port())
	if err != nil {
		t.Fatalf("OpenMetaWithFS: %v", err)
	}
	if len(c.Tracks) != 1 {
		t.Fatalf("Tracks = %d, want 1", len(c.Tracks))
	}
	if c.Tracks[0].Codec != "h264" {
		t.Errorf("Codec = %q, want h264", c.Tracks[0].Codec)
	}

	fetched := fs.BytesFetched()
	if fetched <= 0 {
		t.Error("BytesFetched should be positive")
	}
	if fetched >= int64(len(data)) {
		t.Errorf("BytesFetched = %d, want less than file size %d (windowed read)", fetched, len(data))
	}
}
