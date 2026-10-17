package commands

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/mkvhttp"
)

func TestSEC08_ServeDefaults(t *testing.T) {
	cfg := parseServeFlags([]string{"in.mkv"})
	if cfg.maxWalks != runtime.NumCPU() {
		t.Errorf("default --max-walks %d, want NumCPU %d", cfg.maxWalks, runtime.NumCPU())
	}
	if cfg.cors {
		t.Error("CORS on by default")
	}
	if c := parseServeFlags([]string{"in.mkv", "--max-walks", "0", "--cors"}); c.maxWalks != 0 || !c.cors {
		t.Errorf("--max-walks 0 --cors gave %d, %v", c.maxWalks, c.cors)
	}
	for _, cors := range []bool{false, true} {
		h := mkvhttp.FileHandler("../../../internal/testdata/sample.mkv", cfg.httpOptions(cors))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sample.mkv", nil))
		if got := rec.Header().Get("Access-Control-Allow-Origin") != ""; got != cors {
			t.Errorf("--cors %v: Access-Control-Allow-Origin present %v", cors, got)
		}
	}
}

func TestSEC08_ServeGrowingCORSOptIn(t *testing.T) {
	if parseServeGrowingFlags([]string{"in.mkv"}).cors {
		t.Error("serve-growing: CORS on by default")
	}
	if !parseServeGrowingFlags([]string{"in.mkv", "--cors"}).cors {
		t.Error("serve-growing: --cors ignored")
	}
}
