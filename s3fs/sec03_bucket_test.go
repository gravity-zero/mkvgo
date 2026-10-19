package s3fs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type countingTransport struct{ n atomic.Int64 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Header: http.Header{}}, nil
}

func TestSEC03_BucketHostInjection(t *testing.T) {
	for _, style := range []bool{false, true} {
		fs := New(Options{Region: "eu-west-1", AccessKey: "AKID", SecretKey: "SECRET", SessionToken: "TOKEN", PathStyle: style})
		ct := &countingTransport{}
		fs.client.Transport.(*signingTransport).base = ct
		for _, p := range []string{"s3://evil.example?/k", "s3://a#/k", "s3://A_B/k", "s3://ab/k", "s3://-ab/k", "s3://ab-/k", "s3://a..b/k", "s3://192.168.1.1/k", "s3://evil.example:443@x/k", "s3://" + strings.Repeat("a", 64) + "/k"} {
			if _, err := fs.resolveURL(p); err == nil {
				t.Errorf("pathStyle=%v: %s resolved", style, p)
			}
			if f, err := fs.Port().DoOpen(p); err == nil {
				buf := make([]byte, 1)
				_, _ = f.Read(buf)
				f.Close()
			}
			if _, err := fs.Port().DoStat(p); err == nil {
				t.Errorf("pathStyle=%v: %s stat succeeded", style, p)
			}
		}
		if n := ct.n.Load(); n != 0 {
			t.Fatalf("pathStyle=%v: %d requests left for invalid buckets", style, n)
		}
		for _, ok := range []string{"s3://my-bucket/k", "s3://my.bucket.v2/k", "s3://abc/"} {
			if _, err := fs.resolveURL(ok); err != nil {
				t.Errorf("pathStyle=%v: %s refused: %v", style, ok, err)
			}
		}
	}
}

func TestSEC04_S3RedirectNotFollowed(t *testing.T) {
	var got atomic.Int64
	var leaked atomic.Value
	leaked.Store("")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		leaked.Store(r.Header.Get("Authorization") + "|" + r.Header.Get("x-amz-security-token"))
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	defer origin.Close()
	fs := New(Options{Endpoint: origin.URL, PathStyle: true, AccessKey: "AKID", SecretKey: "SECRET", SessionToken: "TOKEN", AllowHTTPSessionToken: true})
	f, err := fs.Port().DoOpen("s3://mybucket/k.mkv")
	if err == nil {
		buf := make([]byte, 4)
		_, err = f.Read(buf)
		f.Close()
	}
	if err == nil {
		t.Error("a redirect answered a read")
	}
	if _, err := fs.Port().DoStat("s3://mybucket/k.mkv"); err == nil {
		t.Error("a redirect answered a stat")
	}
	if n := got.Load(); n != 0 {
		t.Fatalf("the redirect target got %d requests, credentials %q", n, leaked.Load())
	}
}

// TestKeyDotSegmentsRefused: a key segment "." or ".." would step out of the bucket wherever the URL path is resolved
// (a proxy in front of a path-style endpoint), so it is refused before any request; a dot inside a name is kept.
func TestKeyDotSegmentsRefused(t *testing.T) {
	for _, style := range []bool{false, true} {
		fs := New(Options{Region: "eu-west-1", AccessKey: "AKID", SecretKey: "SECRET", PathStyle: style})
		for _, p := range []string{"s3://abc/../other/k", "s3://abc/x/../../other/k", "s3://abc/./k", "s3://abc/.."} {
			if _, err := fs.resolveURL(p); err == nil {
				t.Errorf("pathStyle=%v: %s resolved", style, p)
			}
		}
		for _, ok := range []string{"s3://abc/a..b/k", "s3://abc/.hidden", "s3://abc/v1.2/k"} {
			if _, err := fs.resolveURL(ok); err != nil {
				t.Errorf("pathStyle=%v: %s refused: %v", style, ok, err)
			}
		}
	}
}

// TestEndpointWithATrailingSlash: AWS_ENDPOINT_URL is often written with a trailing slash; it names the same host,
// where every path was refused as not resolving to "host/".
func TestEndpointWithATrailingSlash(t *testing.T) {
	fs := New(Options{Region: "eu-west-1", AccessKey: "AKID", SecretKey: "SECRET", Endpoint: "http://127.0.0.1:9000/", PathStyle: true})
	u, err := fs.resolveURL("s3://abc/k.mkv")
	if err != nil || u != "http://127.0.0.1:9000/abc/k.mkv" {
		t.Errorf("resolveURL = %q, %v", u, err)
	}
}
