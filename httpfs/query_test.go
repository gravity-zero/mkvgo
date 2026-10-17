package httpfs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestErrorsHideTheQuery: an error never carries a URL's query, where a presigned URL keeps its signature and token.
func TestErrorsHideTheQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer srv.Close()
	const secret = "X-Amz-Signature=deadbeef&X-Amz-Security-Token=tok"
	for name, u := range map[string]string{"404": srv.URL + "/a.mkv?" + secret, "unreachable": "http://127.0.0.1:1/a.mkv?" + secret} {
		err := readFirst(New(), u)
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		if strings.Contains(err.Error(), "deadbeef") || strings.Contains(err.Error(), "tok") {
			t.Errorf("%s: the error shows the query: %v", name, err)
		}
	}
}
