package s3fs

import (
	"strings"
	"testing"
)

// TestSessionTokenRefusedOverPlainHTTP pins that a session token only reaches an http endpoint on opt-in.
func TestSessionTokenRefusedOverPlainHTTP(t *testing.T) {
	const path = "s3://mybucket/k.mkv"
	if _, err := New(Options{Endpoint: "http://127.0.0.1:9000", PathStyle: true, SessionToken: "TOKEN"}).resolveURL(path); err == nil || !strings.Contains(err.Error(), "https://") || !strings.Contains(err.Error(), "AWS_SESSION_TOKEN") {
		t.Errorf("http endpoint with a token: err = %v, want a refusal naming https:// and AWS_SESSION_TOKEN, what a CLI user can change", err)
	}
	if _, err := New(Options{Endpoint: "HTTP://127.0.0.1:9000", PathStyle: true, SessionToken: "TOKEN"}).resolveURL(path); err == nil {
		t.Error("HTTP:// endpoint with a token was accepted")
	}
	for _, o := range []Options{
		{Endpoint: "http://127.0.0.1:9000", PathStyle: true, SessionToken: "TOKEN", AllowHTTPSessionToken: true},
		{Endpoint: "http://127.0.0.1:9000", PathStyle: true},
		{Endpoint: "https://127.0.0.1:9000", PathStyle: true, SessionToken: "TOKEN"},
	} {
		t.Setenv("AWS_SESSION_TOKEN", "")
		if _, err := New(o).resolveURL(path); err != nil {
			t.Errorf("%+v: %v", o, err)
		}
	}
}
