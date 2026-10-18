package commands

import (
	"testing"

	"github.com/gravity-zero/mkvgo/matroska"
)

// TestPatchLanguageClearsBCP47OnlyWhenItChanges pins that a patch repeating a track's language keeps its regional
// tag, and one changing it drops the tag the new language would contradict.
func TestPatchLanguageClearsBCP47OnlyWhenItChanges(t *testing.T) {
	for _, tc := range []struct{ lang, want string }{{"fre", "fr-CA"}, {"eng", ""}} {
		c := &matroska.Container{Tracks: []matroska.Track{{ID: 1, Language: "fre", LanguageBCP47: "fr-CA"}}}
		lang := tc.lang
		applyPatch(c, EditPatch{Tracks: []TrackPatch{{ID: 1, Language: &lang}}})
		if got := c.Tracks[0].LanguageBCP47; got != tc.want {
			t.Errorf("language %q over fre/fr-CA: BCP-47 %q, want %q", tc.lang, got, tc.want)
		}
	}
}
