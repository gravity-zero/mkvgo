package mp4

import (
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestSplitLinksNameTheirCut: two splits of one source at other cuts never link a part of one to a part of the other.
func TestSplitLinksNameTheirCut(t *testing.T) {
	src := editFixture(t)
	a := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 2000}, {StartMs: 2000}}})
	b := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 3000}, {StartMs: 3000}}})
	link := func(p string) splitLink { return readSplitLink(moovBoxesOf(t, p)) }
	if !link(b[1]).continues(link(b[0])) {
		t.Error("a part does not continue the one its split cut before it")
	}
	if link(b[1]).continues(link(a[0])) {
		t.Error("the part after a 3 s cut continues the part before a 2 s cut")
	}
}
