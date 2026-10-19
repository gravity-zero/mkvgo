package s3fs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRegionRedirectSaysWhichRegion: a bucket answered from another region names it, so the caller knows what to set.
func TestRegionRedirectSaysWhichRegion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-amz-bucket-region", "eu-west-3")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer srv.Close()
	fs := New(Options{Region: "us-east-1", Endpoint: srv.URL, PathStyle: true, AccessKey: "AKID", SecretKey: "SECRET"})
	f, err := fs.Port().DoOpen("s3://mybucket/movie.mkv")
	if err == nil {
		defer f.Close()
		_, err = f.Read(make([]byte, 4)) // an open is lazy: the first read asks the server
	}
	if err == nil || !strings.Contains(err.Error(), "eu-west-3") {
		t.Errorf("err = %v, want one naming the bucket's region", err)
	}
}
