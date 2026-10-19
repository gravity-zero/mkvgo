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

// TestSplitLinksCarryTheirCut: each part but the last names how far into it the split cut, so a join places the next
// part there without reading it off the tracks; the last runs to the source's end and names none.
func TestSplitLinksCarryTheirCut(t *testing.T) {
	src := editFixture(t)
	parts := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 2000}, {StartMs: 2000, EndMs: 4000}, {StartMs: 4000}}})
	var cuts []int64
	for _, p := range parts {
		cuts = append(cuts, readSplitLink(moovBoxesOf(t, p)).cutNs)
	}
	if cuts[0] <= 0 || cuts[1] <= 0 || cuts[2] != 0 {
		t.Fatalf("cuts %v, want two positive then none", cuts)
	}
	m := mustCutModel(t, parts[0])
	if got := m.tracks[m.ref].ptNs(0, m.movieTS) + cuts[0]; roundMs(got) < 1900 || roundMs(got) > 2100 {
		t.Errorf("part 1 is cut %d ms in, want about 2000", roundMs(cuts[0]))
	}
}
