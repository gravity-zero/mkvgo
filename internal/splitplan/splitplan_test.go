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

// TestCheckRanges: a range ending before it starts, or starting past the end, is refused by its number.
func TestCheckRanges(t *testing.T) {
	for _, tc := range []struct {
		r    []mkv.TimeRange
		want string
	}{
		{[]mkv.TimeRange{{StartMs: 0, EndMs: 1000}, {StartMs: 3000, EndMs: 3000}}, "range 2 ends at 3000 ms, before it starts"},
		{[]mkv.TimeRange{{StartMs: 7000}}, "range 1 starts at 7000 ms, past the end"},
		{[]mkv.TimeRange{{StartMs: 0, EndMs: 9000}, {StartMs: 5000}}, ""},
	} {
		err := CheckRanges(tc.r, 6000)
		if (tc.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ranges %v: err = %v, want %q", tc.r, err, tc.want)
		}
	}
	if err := CheckRanges([]mkv.TimeRange{{StartMs: 7000}}, 0); err != nil {
		t.Errorf("an unknown duration refused a range: %v", err)
	}
}

// TestChapterRangesLeaveNoGap: stated ends that leave a gap, or stop before the end, still put every instant in a part.
func TestChapterRangesLeaveNoGap(t *testing.T) {
	r := ChapterRanges([]mkv.Chapter{{StartMs: 0, EndMs: 1000}, {StartMs: 2000, EndMs: 3000}, {StartMs: 4000, EndMs: 5000}})
	want := []mkv.TimeRange{{StartMs: 0, EndMs: 2000}, {StartMs: 2000, EndMs: 4000}, {StartMs: 4000, EndMs: 0}}
	for i := range want {
		if r[i] != want[i] {
			t.Errorf("range %d = %+v, want %+v", i, r[i], want[i])
		}
	}
	if o := ChapterRanges([]mkv.Chapter{{StartMs: 0, EndMs: 1500}, {StartMs: 1000}}); o[0].EndMs != 1500 {
		t.Errorf("an overlap a chapter states became %+v", o[0])
	}
}
