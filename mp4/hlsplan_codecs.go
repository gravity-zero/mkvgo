package mp4

import "github.com/gravity-zero/mkvgo/mkv"

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
