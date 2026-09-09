package mp4

import (
	"strings"
	"testing"
)

// The drop reasons are operator-facing text: a refusal has to say what to do
// next, not only that something failed. A PGS track is not unusable - it is
// unusable THIS WAY - so the message names the path that works.
func TestSubtitleDropReason_IsActionable(t *testing.T) {
	cases := []struct {
		codec string
		want  []string
	}{
		{"pgs", []string{"ExtractSubtitlePGS", "no sample entry"}},
		{"vobsub", []string{"cannot be carried"}},
		{"ass", []string{"FlattenStyledSubs"}},
	}
	for _, tc := range cases {
		got := subtitleDropReason(tc.codec)
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("subtitleDropReason(%q) = %q, want it to mention %q", tc.codec, got, want)
			}
		}
	}
}
