package commands

import "testing"

// TestServeListensOnLoopbackByDefault pins that serve and serve-growing only open to the network on -addr.
func TestServeListensOnLoopbackByDefault(t *testing.T) {
	if got := parseServeFlags([]string{"movie.mkv"}).addr; got != "127.0.0.1:8478" {
		t.Errorf("serve default addr = %q, want 127.0.0.1:8478", got)
	}
	if got := parseServeGrowingFlags([]string{"movie.mkv"}).addr; got != "127.0.0.1:8478" {
		t.Errorf("serve-growing default addr = %q, want 127.0.0.1:8478", got)
	}
	if got := parseServeFlags([]string{"-addr", ":9000", "movie.mkv"}).addr; got != ":9000" {
		t.Errorf("serve -addr :9000 = %q", got)
	}
}
