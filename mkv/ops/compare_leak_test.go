package ops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/httpfs"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestCompareConcatHidesSignedURLs: a part of a concat comparison may be a presigned URL; the error naming a part
// that does not open shows it without its query, as every other error naming a source does.
func TestCompareConcatHidesSignedURLs(t *testing.T) {
	data, err := os.ReadFile("../../internal/testdata/sample.mkv")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/gone") {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		http.ServeContent(w, r, "s.mkv", time.Time{}, strings.NewReader(string(data)))
	}))
	defer srv.Close()
	fs := httpfs.New().Port()
	const sig = "?X-Amz-Signature=deadbeef"
	_, err = CompareBlocksConcat(context.Background(), srv.URL+"/s.mkv"+sig, []string{srv.URL + "/gone.mkv" + sig}, mkv.Options{FS: fs})
	if err == nil || strings.Contains(err.Error(), "deadbeef") {
		t.Errorf("CompareBlocksConcat with an expired part: %v, want an error without the signature", err)
	}
}
