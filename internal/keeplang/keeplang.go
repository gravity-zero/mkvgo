// Package keeplang resolves a list of languages to the tracks to keep: every video track and the tracks in one of
// those languages, the same way for the CLI's --keep-lang and the wasm's keepLangs.
package keeplang

import (
	"slices"
	"strings"

	"github.com/gravity-zero/mkvgo/internal/lang"
	"github.com/gravity-zero/mkvgo/mkv"
)

// IDs is every video track plus the tracks in one of langs: by the legacy code or the BCP 47 tag as written
// ("und" included), by the primary language of either, or by the language the container resolves (the Matroska
// default "eng" for a track stating none). Matching the legacy code alone kept the video alone for a BCP 47-only
// or a default-language track.
func IDs(c *mkv.Container, langs []string) []uint64 {
	exact, primary := make(map[string]bool, len(langs)), make(map[string]bool, len(langs))
	for _, l := range langs {
		l = strings.ToLower(strings.TrimSpace(l))
		exact[l] = true
		if p := lang.Primary(l); p != "und" {
			primary[p] = true
		}
	}
	var ids []uint64
	for i := range c.Tracks {
		t := &c.Tracks[i]
		stated := []string{t.Language, t.LanguageBCP47, c.TrackLanguage(t)}
		if t.Type == mkv.VideoTrack || slices.ContainsFunc(stated, func(l string) bool {
			return l != "" && (exact[strings.ToLower(l)] || primary[lang.Primary(l)])
		}) {
			ids = append(ids, t.ID)
		}
	}
	return ids
}
