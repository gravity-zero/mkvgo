package mp4

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/httpfs"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestSignedURLStaysOutOfErrors: an error naming a remote source showed its whole URL, the signature of a presigned
// one included, to whoever reads the message. It names the URL without its query now - from the planner, the reader
// and a concat part alike.
func TestSignedURLStaysOutOfErrors(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var blocks []genBlock
	for i := 0; i < 50; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: []byte{0, 0, 0, 1, 0x65, byte(i)}})
	}
	video := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}
	data, err := os.ReadFile(pkgcovNoCuesMKV(t, video, blocks))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gone.mkv" {
			http.Error(rw, "expired", http.StatusForbidden)
			return
		}
		http.ServeContent(rw, r, "s.mkv", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	const sig = "?X-Amz-Signature=deadbeef"
	fs := httpfs.New().Port()
	ctx := context.Background()

	_, planErr := PlanHLS(ctx, srv.URL+"/s.mkv"+sig, Options{SegmentMs: 2000, FS: fs})
	_, openErr := reader.OpenWithFS(ctx, srv.URL+"/gone.mkv"+sig, fs)
	_, concatErr := PlanConcat(ctx, []string{srv.URL + "/s.mkv" + sig, srv.URL + "/s.mkv" + sig}, Options{SegmentMs: 2000, FS: fs})
	for name, err := range map[string]error{"PlanHLS": planErr, "Open": openErr, "PlanConcat": concatErr} {
		if err == nil {
			t.Errorf("%s: no error, want one naming the source", name)
		} else if strings.Contains(err.Error(), "deadbeef") {
			t.Errorf("%s: the signature reached the error: %v", name, err)
		}
	}
}
