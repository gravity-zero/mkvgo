package commands

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkvhttp"
)

// TestServeDirectNamesWithSpacesAndBraces: a film's name holds spaces, parentheses or braces; serve --direct mounts
// it at that name and the URL it prints reaches it, where a ServeMux pattern panicked on the space.
func TestServeDirectNamesWithSpacesAndBraces(t *testing.T) {
	for _, name := range []string{"Mon Film (2020).mkv", "a{b}.mkv", "plain.mkv", "50% off #1.mkv"} {
		src := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(src, []byte("film bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(directMux(name, mkvhttp.FileHandler(src, mkvhttp.Options{})))
		u := directPlayableURL(strings.TrimPrefix(srv.URL, "http://"), name)
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "film bytes" {
			t.Errorf("%q at %s: status %d, %q", name, u, resp.StatusCode, body)
		}
		if r, err := http.Get(srv.URL + "/other.mkv"); err != nil || r.StatusCode != http.StatusNotFound {
			t.Errorf("%q: another name was served", name)
		}
		srv.Close()
	}
}
