package mp4

import (
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// PlanCodec is one track of an HLSPlan with the RFC 6381 codec string the plan's playlists name it by.
type PlanCodec struct {
	TrackID uint64        // the track's number in the source
	Type    mkv.TrackType // video or audio
	Codec   string        // the codec as packaged ("hevc", "dts")
	RFC6381 string        // the string as a CODECS attribute writes it ("hvc1.2.4.H153.B0"); "" when Known is false
	Known   bool          // false: the codec has no string here, and a CODECS that would name it is left out whole
}

// Codecs lists the plan's video and audio tracks in the order of its renditions, the video first, each with its
// RFC 6381 string exactly as the master playlist writes it: sample entry prefix (hvc1 or hev1, avc1 or avc3)
// included, and for a track Options.DolbyVision converted, the string of what is served. The master leaves CODECS
// out of a variant when one of its tracks has no string (a partial list is worse than none); the list still names
// every track that has one. Subtitle renditions carry no codec string and are not listed.
func (p *HLSPlan) Codecs() []PlanCodec {
	out := make([]PlanCodec, 0, len(p.tracks))
	for _, pt := range p.tracks {
		ot := pt.ft.outTrack
		s := rfc6381Codec(ot)
		out = append(out, PlanCodec{TrackID: ot.mkv.ID, Type: ot.mkv.Type, Codec: ot.mkv.Codec, RFC6381: s, Known: s != ""})
	}
	return out
}

// entryPairs are the sample entry names a caller may choose between: parameter sets in the record, or in the samples.
var entryPairs = map[string]string{"hvc1": "hev1", "hev1": "hvc1", "avc1": "avc3", "avc3": "avc1"}

// CodecString returns the RFC 6381 string of a track for a caller that packages it without a plan, built by the
// code HLSPlan.Codecs reads. entry names the sample entry that caller writes ("hvc1" or "hev1", "avc1" or "avc3")
// and replaces the other name of its pair at the head of the string; "" keeps the name this package would write,
// and an entry of the other pair leaves the string alone, so one value serves every track of a file. The track is
// described as served: hand in the track after any Dolby Vision conversion. known is false, and the string empty,
// when no string can be given: a codec without one (DTS), a missing or short configuration record, VP9 without its
// record (a plan reads it from the first keyframe), an entry that is none of the four.
func CodecString(t *mkv.Track, entry string) (rfc6381 string, known bool) {
	other, ok := entryPairs[entry]
	if t == nil || (entry != "" && !ok) {
		return "", false
	}
	s := rfc6381Codec(&outTrack{mkv: *t})
	if rest, cut := strings.CutPrefix(s, other+"."); cut && entry != "" {
		s = entry + "." + rest
	}
	return s, s != ""
}
