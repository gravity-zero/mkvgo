package mp4

// manifest_attrs.go - what the HLS masters (hls.go, abr.go, concat.go, cmafhls.go) and the DASH manifests (dash.go,
// singlefile.go, cmafmpd.go) say of a track beyond its codec: dynamic range and Dolby Vision, accessibility and
// roles, Atmos, and the audio groups. One place, so every manifest says the same of the same track.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// rendition is a track as a manifest describes it: the track, its RFC 6381 codec string ("" unknown), and the
// complexity index of its Dolby Atmos (E-AC-3 JOC) objects, 0 for none.
type rendition struct {
	t     *mkv.Track
	codec string
	joc   uint8
}

// renditionOf describes an output track, its Atmos read from the dec3 its init carries.
func renditionOf(ot *outTrack) rendition {
	return rendition{t: &ot.mkv, codec: rfc6381Codec(ot), joc: entryJOC(ot.sampleEntry)}
}

// renditionName is the NAME a rendition shows: its title, else its language, else kind and number.
func renditionName(t *mkv.Track, kind string, n int) string {
	if t.Name != "" {
		return t.Name
	}
	if l := t.ResolvedLanguage(); l != "" {
		return l
	}
	return fmt.Sprintf("%s %d", kind, n)
}

// uniqueNames makes each NAME of one group unique, as HLS requires: a repeated one (untitled tracks of one
// language) gets " 2", " 3", the first keeping its own.
func uniqueNames(names []string) []string {
	used := map[string]bool{}
	out := make([]string, len(names))
	for i, n := range names {
		name := n
		for k := 2; used[name]; k++ {
			name = fmt.Sprintf("%s %d", n, k)
		}
		used[name], out[i] = true, name
	}
	return out
}

// groupNames is the NAME of each audio rendition (audio index order), unique within its group.
func groupNames(audio []rendition, groups []audioGroup) []string {
	out := make([]string, len(audio))
	for _, g := range groups {
		names := make([]string, len(g.members))
		for j, k := range g.members {
			names[j] = renditionName(audio[k].t, "Audio", k+1)
		}
		for j, n := range uniqueNames(names) {
			out[g.members[j]] = n
		}
	}
	return out
}

// subtitleNames is the NAME of each subtitle rendition, unique within the "subs" group.
func subtitleNames(tracks []*mkv.Track) []string {
	names := make([]string, len(tracks))
	for i, t := range tracks {
		names[i] = renditionName(t, "Subtitles", i+1)
	}
	return uniqueNames(names)
}

// hlsAudioMedia is the EXT-X-MEDIA line of an audio rendition named name in group, at uri.
func hlsAudioMedia(r rendition, name string, isDefault bool, group, uri string) string {
	t := r.t
	attrs := fmt.Sprintf("TYPE=AUDIO,GROUP-ID=%s,NAME=%s,AUTOSELECT=YES", hlsQuoted(group), hlsQuoted(name))
	if l := t.ResolvedLanguage(); l != "" {
		attrs += fmt.Sprintf(",LANGUAGE=%s", hlsQuoted(l))
	}
	if r.joc > 0 {
		attrs += fmt.Sprintf(",CHANNELS=\"%d/JOC\"", r.joc) // Atmos: the object complexity, as HLS states it
	} else {
		attrs += hlsChannelsAttr(t)
	}
	if c := hlsCharacteristics(t, false); c != "" {
		attrs += ",CHARACTERISTICS=" + hlsQuoted(c)
	}
	if isDefault {
		attrs += ",DEFAULT=YES"
	}
	return fmt.Sprintf("#EXT-X-MEDIA:%s%s,URI=%s\n", attrs, trackAttr(t), hlsURI(uri))
}

// subtitleDefault is the subtitle track DEFAULT=YES goes to: the first one flagged default (a track without
// FlagDefault reads as flagged), -1 for none. HLS allows one per group.
func subtitleDefault(tracks []*mkv.Track) int {
	for i, t := range tracks {
		if t.IsDefault {
			return i
		}
	}
	return -1
}

// hlsSubtitleMedia is the EXT-X-MEDIA line of a subtitle rendition named name, group "subs", at uri.
func hlsSubtitleMedia(t *mkv.Track, name string, isDefault bool, uri string) string {
	attrs := fmt.Sprintf("TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=%s,AUTOSELECT=YES", hlsQuoted(name))
	if l := t.ResolvedLanguage(); l != "" {
		attrs += fmt.Sprintf(",LANGUAGE=%s", hlsQuoted(l))
	}
	if c := hlsCharacteristics(t, true); c != "" {
		attrs += ",CHARACTERISTICS=" + hlsQuoted(c)
	}
	if isDefault {
		attrs += ",DEFAULT=YES"
	}
	if t.IsForced {
		attrs += ",FORCED=YES"
	}
	return fmt.Sprintf("#EXT-X-MEDIA:%s%s,URI=%s\n", attrs, trackAttr(t), hlsURI(uri))
}

// trackAttr is the X-MKVGO-TRACK client attribute (RFC 8216 4.4.6.1: players ignore it) naming a rendition's source
// track by the number every mkvgo command and the probe use (the Matroska TrackNumber, an MP4 track's 1-based
// position): what an integrator pairs a rendition with its own track by, whatever its NAME, its group or the tracks
// left out. "" for a track without one.
func trackAttr(t *mkv.Track) string {
	if t.ID == 0 {
		return ""
	}
	return fmt.Sprintf(",X-MKVGO-TRACK=\"%d\"", t.ID)
}

// hlsCharacteristics is the CHARACTERISTICS of a rendition from its disposition flags, the Apple accessibility
// characteristics, "" for none. A forced subtitle says so through FORCED; a commentary has no HLS characteristic.
func hlsCharacteristics(t *mkv.Track, text bool) string {
	var c []string
	switch {
	case text && t.HearingImpaired:
		c = append(c, "public.accessibility.transcribes-spoken-dialog", "public.accessibility.describes-music-and-sound")
	case t.HearingImpaired:
		c = append(c, "public.accessibility.enhances-speech-intelligibility")
	}
	if t.VisualImpaired || (text && t.TextDescriptions) {
		c = append(c, "public.accessibility.describes-video")
	}
	return strings.Join(c, ",")
}

// hlsVideoAttrs are the EXT-X-STREAM-INF attributes of a variant's video: RESOLUTION, FRAME-RATE, VIDEO-RANGE (PQ,
// HLG; SDR, the default, unsaid) and, for a cross-compatible Dolby Vision stream, SUPPLEMENTAL-CODECS.
func hlsVideoAttrs(t *mkv.Track) string {
	var a string
	if t.Width != nil && t.Height != nil && *t.Width > 0 && *t.Height > 0 {
		a += fmt.Sprintf(",RESOLUTION=%dx%d", *t.Width, *t.Height)
	}
	if t.FrameRate != nil && *t.FrameRate > 0 {
		a += fmt.Sprintf(",FRAME-RATE=%.3f", *t.FrameRate)
	}
	if r := videoRange(t); r != "" {
		a += ",VIDEO-RANGE=" + r
	}
	if s := dvSupplementalCodec(t); s != "" {
		a += ",SUPPLEMENTAL-CODECS=" + hlsQuoted(s)
	}
	return a
}

// videoRange is the HLS VIDEO-RANGE of a video track: PQ or HLG from its transfer characteristics or its Dolby Vision
// base layer, "" for SDR or unknown.
func videoRange(t *mkv.Track) string {
	if dv := t.DolbyVision; dv != nil {
		switch dv.BLSignalCompatID {
		case 0, 1, 6: // profile 5 (IPT-PQ), HDR10, BT.2100 PQ
			return "PQ"
		case 4:
			return "HLG"
		}
	}
	if t.ColorTransfer != nil {
		switch *t.ColorTransfer {
		case 16:
			return "PQ"
		case 18:
			return "HLG"
		}
	}
	return ""
}

// dvBrands is the compatibility brand of each cross-compatible Dolby Vision base layer (bl_signal_compatibility_id).
var dvBrands = map[uint8]string{1: "db1p", 2: "db2g", 4: "db4h"}

// dvSupplementalCodec is the SUPPLEMENTAL-CODECS of a Dolby Vision stream whose base layer plays without it: the
// Dolby string and the base layer's brand, dvh1.08.06/db1p; "" for another stream.
func dvSupplementalCodec(t *mkv.Track) string {
	dv := t.DolbyVision
	if dv == nil {
		return ""
	}
	et, brand := dvEntryTypes[t.Codec], dvBrands[dv.BLSignalCompatID]
	if et == "" || brand == "" {
		return ""
	}
	return fmt.Sprintf("%s.%02d.%02d/%s", et, dv.Profile, dv.Level, brand)
}

// audioGroup is one HLS audio group: the renditions of one codec (Atmos apart), which a variant names together with
// its codec string.
type audioGroup struct {
	id      string
	codec   string // "" when a member's codec string is unknown
	members []int  // indexes into the renditions
}

// audioGroupsOf splits audio renditions into one group per codec - a player that cannot decode one codec picks the
// variant of another - when every language is in every group: a player lists only its variant's group, so a split
// that left a language in one codec alone would hide it from the others. Otherwise, or with a single codec, one group
// "aud" holds them all, its codec string every codec they use.
func audioGroupsOf(audio []rendition) []audioGroup {
	groups := splitByCodec(audio)
	if len(groups) > 1 && !sameLanguages(audio, groups) {
		one := audioGroup{id: "aud"}
		var codecs []string
		for i, r := range audio {
			one.members = append(one.members, i)
			if !slices.Contains(codecs, r.codec) {
				codecs = append(codecs, r.codec)
			}
		}
		one.codec = joinKnown(codecs)
		return []audioGroup{one}
	}
	return groups
}

// sameLanguages reports whether every group holds the same set of languages.
func sameLanguages(audio []rendition, groups []audioGroup) bool {
	langs := func(g audioGroup) []string {
		var l []string
		for _, i := range g.members {
			if x := audio[i].t.ResolvedLanguage(); !slices.Contains(l, x) {
				l = append(l, x)
			}
		}
		slices.Sort(l)
		return l
	}
	first := langs(groups[0])
	for _, g := range groups[1:] {
		if !slices.Equal(langs(g), first) {
			return false
		}
	}
	return true
}

// splitByCodec groups audio renditions by codec string (Atmos apart), "aud-<codec>" each, "aud" for a single one.
func splitByCodec(audio []rendition) []audioGroup {
	var groups []audioGroup
	byKey := map[string]int{}
	for i, r := range audio {
		key := r.codec
		if r.joc > 0 {
			key += "/JOC"
		}
		g, ok := byKey[key]
		if !ok {
			g = len(groups)
			byKey[key] = g
			groups = append(groups, audioGroup{codec: r.codec})
		}
		groups[g].members = append(groups[g].members, i)
	}
	for i := range groups {
		groups[i].id = "aud"
		if len(groups) > 1 {
			groups[i].id = "aud-" + groupSlug(groups[i].codec, audio[groups[i].members[0]].joc > 0, i)
		}
	}
	return groups
}

// groupSlug is a GROUP-ID suffix from a codec string: its letters, digits and dots, "-joc" for Atmos.
func groupSlug(codec string, joc bool, i int) string {
	s := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return -1
	}, codec)
	if s == "" {
		s = fmt.Sprintf("%d", i+1)
	}
	if joc {
		s += "-joc"
	}
	return s
}

// groupDefaults says which member of each group is DEFAULT=YES: the flagged default when the group has one, else its
// first.
func groupDefaults(audio []rendition, groups []audioGroup) map[int]bool {
	def := map[int]bool{}
	for _, g := range groups {
		chosen := -1
		for _, i := range g.members {
			if audio[i].t.IsDefault {
				chosen = i
				break
			}
		}
		if chosen < 0 {
			chosen = g.members[0]
		}
		def[chosen] = true
	}
	return def
}

// joinKnown joins codec strings with commas, "" when one of them is unknown.
func joinKnown(codecs []string) string {
	for _, c := range codecs {
		if c == "" {
			return ""
		}
	}
	return strings.Join(codecs, ",")
}

// joinCodecs is the CODECS of a variant: the video's string and its audio group's, "" when one is unknown (a partial
// list is worse than none).
func joinCodecs(video, audio string, hasAudio bool) string {
	if video == "" || (hasAudio && audio == "") {
		return ""
	}
	if !hasAudio {
		return video
	}
	return video + "," + audio
}

// dashVideoProperties are a video AdaptationSet's SupplementalProperty elements for its colour code points
// (ISO/IEC 23001-8 CICP: primaries, transfer, matrix), one line each, "" when none is known.
func dashVideoProperties(t *mkv.Track, indent string) string {
	var b strings.Builder
	for _, p := range []struct {
		scheme string
		v      *uint16
	}{
		{"urn:mpeg:mpegB:cicp:ColourPrimaries", t.ColorPrimaries},
		{"urn:mpeg:mpegB:cicp:TransferCharacteristics", t.ColorTransfer},
		{"urn:mpeg:mpegB:cicp:MatrixCoefficients", t.ColorSpace},
	} {
		if p.v != nil && *p.v != 2 { // 2: unspecified
			fmt.Fprintf(&b, `%s<SupplementalProperty schemeIdUri="%s" value="%d"/>`+"\n", indent, p.scheme, *p.v)
		}
	}
	return b.String()
}

// dashDescriptors are the descriptors of an audio or subtitle AdaptationSet in the schema's order: the Atmos
// SupplementalProperty, the Label, then Accessibility and Role.
func dashDescriptors(r rendition, text bool, indent string) string {
	return dashJOCProperty(r, indent) + dashLabel(r.t, indent) + dashRoles(r.t, text, indent)
}

// dashJOCProperty is an E-AC-3 AdaptationSet's SupplementalProperty announcing Dolby Atmos (JOC), "" without it.
func dashJOCProperty(r rendition, indent string) string {
	if r.joc == 0 {
		return ""
	}
	return fmt.Sprintf(`%s<SupplementalProperty schemeIdUri="tag:dolby.com,2018:dash:EC3_ExtensionType:2018" value="JOC"/>`+"\n", indent)
}

// dashRoles are an AdaptationSet's Accessibility and Role elements from its track's disposition flags (schema order:
// Accessibility, then Role), "" when it has none. text says a subtitle track.
func dashRoles(t *mkv.Track, text bool, indent string) string {
	var b strings.Builder
	const purpose = "urn:tva:metadata:cs:AudioPurposeCS:2007"
	if !text && t.VisualImpaired {
		fmt.Fprintf(&b, `%s<Accessibility schemeIdUri="%s" value="1"/>`+"\n", indent, purpose)
	}
	if !text && t.HearingImpaired {
		fmt.Fprintf(&b, `%s<Accessibility schemeIdUri="%s" value="2"/>`+"\n", indent, purpose)
	}
	var role string
	switch {
	case t.Commentary:
		role = "commentary"
	case text && t.IsForced:
		role = "forced-subtitle"
	case text && t.HearingImpaired:
		role = "caption"
	case text && t.TextDescriptions, !text && t.VisualImpaired:
		role = "description"
	case text:
		role = "subtitle"
	case t.IsDefault:
		role = "main"
	}
	if role != "" {
		fmt.Fprintf(&b, `%s<Role schemeIdUri="urn:mpeg:dash:role:2011" value="%s"/>`+"\n", indent, role)
	}
	return b.String()
}

// bandwidthOf is the peak and average bit rate over segs of the tracks at main (fts indexes) and the heaviest, in each
// segment, of the tracks at alts - a variant's audio group, of which a player plays one at a time - from each
// segment's bytes per track; the whole segment's when those are unknown (a Matroska plan's estimate).
func bandwidthOf(segs []segInfo, main, alts []int) (peak, avg int64) {
	var p, bits, secs float64
	for _, s := range segs {
		b := s.bytes
		if s.perTrack != nil {
			b = 0
			for _, i := range main {
				if i >= 0 && i < len(s.perTrack) {
					b += s.perTrack[i]
				}
			}
			var heaviest int64
			for _, i := range alts {
				if i >= 0 && i < len(s.perTrack) {
					heaviest = max(heaviest, s.perTrack[i])
				}
			}
			b += heaviest
		}
		if s.durSec > 0 {
			p = max(p, float64(b)*8/s.durSec)
		}
		bits += float64(b) * 8
		secs += s.durSec
	}
	if secs <= 0 {
		return int64(p), int64(p)
	}
	return int64(p), int64(bits / secs)
}

// groupAt is the fts indexes of a group's members (at maps a rendition to its fts index).
func groupAt(g audioGroup, at []int) []int {
	idx := make([]int, len(g.members))
	for j, k := range g.members {
		idx[j] = at[k]
	}
	return idx
}

// videoBandwidth is the peak bit rate of the video rendition alone, what its DASH Representation announces (the
// audio has AdaptationSets of its own); the whole segments' when the bytes per track are unknown.
func videoBandwidth(segs []segInfo, fts []*fragTrack) int64 {
	peak, _ := bandwidthOf(segs, []int{slices.Index(fts, pickVideoFrag(fts))}, nil)
	return peak
}
