package httpfs

import (
	"net/http"
	"testing"
)

// TestRedirectDowngradeRefused: a redirect from https to http on the same host is refused while Options.Header is
// set, since the custom headers would cross the network in clear.
func TestRedirectDowngradeRefused(t *testing.T) {
	from, _ := http.NewRequest("GET", "https://media.example/a.mkv", nil)
	to, _ := http.NewRequest("GET", "http://media.example/a.mkv", nil)
	policy := redirectPolicy(Options{Header: http.Header{"X-Api-Key": {"k"}}})
	if err := policy(to, []*http.Request{from}); err == nil {
		t.Error("an https to http redirect was followed with the caller's headers")
	}
	if err := redirectPolicy(Options{})(to, []*http.Request{from}); err != nil {
		t.Errorf("without headers the downgrade carries nothing of the caller's: %v", err)
	}
}
