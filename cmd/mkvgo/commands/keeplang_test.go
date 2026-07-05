package commands

import (
	"reflect"
	"testing"
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
