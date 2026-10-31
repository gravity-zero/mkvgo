package commands

import (
	"reflect"
	"testing"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestResolveKeepLangs checks the --keep-lang → KeepTracks resolution: every
// video track plus the audio/subtitle tracks matching the requested languages.
func TestResolveKeepLangs(t *testing.T) {
	src := "../../../internal/testdata/regfix.mkv" // video(1,und) + fre audio(2) + eng audio(3)
	cases := []struct {
		langs []string
		want  []uint64
	}{
		{[]string{"fre"}, []uint64{1, 2}}, // video + French audio
		{[]string{"eng"}, []uint64{1, 3}}, // video + English audio
		{[]string{"fre", "eng"}, []uint64{1, 2, 3}},
		{[]string{"spa"}, []uint64{1}}, // no match → video only
	}
	for _, c := range cases {
		got := resolveKeepLangs(src, c.langs)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("resolveKeepLangs(%v) = %v, want %v", c.langs, got, c.want)
		}
	}
}

// TestKeepLangIDsUsesTheResolvedLanguage: a track stating no language is English by the Matroska default, a BCP 47
// tag matches by its primary subtag; both kept the video alone before.
func TestKeepLangIDsUsesTheResolvedLanguage(t *testing.T) {
	c := &matroska.Container{DocType: "matroska", Tracks: []matroska.Track{
		{ID: 1, Type: mkv.VideoTrack},
		{ID: 2, Type: mkv.AudioTrack},                         // no Language element: eng by default
		{ID: 3, Type: mkv.AudioTrack, LanguageBCP47: "fr-CA"}, // BCP 47 only
		{ID: 4, Type: mkv.AudioTrack, Language: "spa"},        // legacy only
		{ID: 5, Type: mkv.SubtitleTrack, LanguageBCP47: "de"},
		{ID: 6, Type: mkv.AudioTrack, Language: "und"},
	}}
	for _, tc := range []struct {
		langs []string
		want  []uint64
	}{
		{[]string{"eng"}, []uint64{1, 2}},
		{[]string{"fr"}, []uint64{1, 3}},
		{[]string{"fr-ca"}, []uint64{1, 3}},
		{[]string{"spa", "de"}, []uint64{1, 4, 5}},
		{[]string{"und"}, []uint64{1, 6}}, // "und" as written, which the primary-language match alone dropped
		{[]string{"ita"}, []uint64{1}},
	} {
		if got := keepLangIDs(c, tc.langs); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("keepLangIDs(%v) = %v, want %v", tc.langs, got, tc.want)
		}
	}
}
