package splitplan

import (
	"strings"
	"testing"
)

// TestCheckPatternExampleKeepsTheExtension: a refused pattern's example names a file of the container being split.
func TestCheckPatternExampleKeepsTheExtension(t *testing.T) {
	for _, ext := range []string{".mkv", ".mp4"} {
		err := CheckPattern("out/part_%03d" + ext)
		if err == nil || !strings.Contains(err.Error(), "e.g. part_%03d"+ext) {
			t.Errorf("%s: %v", ext, err)
		}
	}
}
