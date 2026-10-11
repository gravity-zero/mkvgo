package mp4

import (
	"context"
	"testing"
)

// SourceID names what the URLs serve, not only the file: two packagings of
// one file give the same segment names other bytes, and so other SourceIDs.
func TestSourceIDFollowsPackaging(t *testing.T) {
	src := openGOPSource(t, 6, false, true)
	ctx := context.Background()
	id := func(o Options) string {
		p, err := PlanHLS(ctx, src, o)
		if err != nil {
			t.Fatal(err)
		}
		return p.SourceID()
	}
	a, b := id(Options{SegmentMs: openGOPMs}), id(Options{SegmentMs: openGOPMs})
	if a == "" || a != b {
		t.Fatalf("one file, one packaging: %q vs %q", a, b)
	}
	if c := id(Options{SegmentMs: 2 * openGOPMs}); c == a {
		t.Errorf("another segment duration kept the SourceID %q", a)
	}
	if d := id(Options{SegmentMs: openGOPMs, VideoOnly: true}); d == a {
		t.Errorf("another track selection kept the SourceID %q", a)
	}
}
