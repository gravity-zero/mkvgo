package splitplan

import (
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
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

// TestChapterRangesOpenOnTheHead: the first chapter's range starts at 0, so what plays before it is in a part.
func TestChapterRangesOpenOnTheHead(t *testing.T) {
	r := ChapterRanges([]mkv.Chapter{{StartMs: 2500}, {StartMs: 500}})
	if r[1].StartMs != 0 || r[1].EndMs != 2500 || r[0].StartMs != 2500 {
		t.Errorf("ranges %+v", r)
	}
}
