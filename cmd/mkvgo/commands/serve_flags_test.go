package commands

import (
	"runtime"
	"testing"
)

// The default --max-walks is the processors the process may use (GOMAXPROCS, which follows a container's CPU
// limit), not the host's cores.
func TestServeMaxWalksFollowsGOMAXPROCS(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	if got := parseServeFlags([]string{"movie.mkv"}).maxWalks; got != 2 {
		t.Fatalf("default max walks under GOMAXPROCS 2: %d", got)
	}
}
