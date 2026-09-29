package reader

import "github.com/gravity-zero/mkvgo/mkv"

// fillCodecDerived fills the track fields that follow from the codec itself
// or from its configuration record once the container has had its say: the
// VP9 level (fillVP9Level), the scan type of codecs that have no interlaced
// coding tools (fillProgressiveOnly), the AAC profile and SBR output rate
// (fillAACFromASC). Called after fillColourFromCodecPrivate on every reader
// path; container values always win per field.
func fillCodecDerived(t *mkv.Track) {
	fillVP9Level(t)
	fillProgressiveOnly(t)
	fillAACFromASC(t)
}

// FillFromCodecPrivate fills any nil field of a track from its codec
// bitstream and from what the codec dictates - colour and profile/level from
// the configuration record (FillColourFromCodecPrivate), then the derived
// facts fillCodecDerived lists. Exported for probes that assemble tracks
// outside the reader (the mp4 package). Safe on any input.
func FillFromCodecPrivate(t *mkv.Track) {
	fillColourFromCodecPrivate(t)
	fillCodecDerived(t)
}

// progressiveOnlyCodec reports whether the codec has no interlaced coding at
// all - VP8, VP9 and AV1 code progressive frames only - so its scan type is a
// property of the format, not something the file has to state.
func progressiveOnlyCodec(codec string) bool {
	switch codec {
	case "vp8", "V_VP8", "vp9", "V_VP9", "av1", "V_AV1":
		return true
	}
	return false
}

// fillProgressiveOnly sets the scan type and field order of a video track whose
// codec cannot be interlaced, when the container left them unstated. A
// container that does state them (FlagInterlaced) keeps its word, even a wrong
// one - the reader reports, it does not correct.
func fillProgressiveOnly(t *mkv.Track) {
	if t.Type != mkv.VideoTrack || t.ScanType != "" || !progressiveOnlyCodec(t.Codec) {
		return
	}
	t.ScanType = "progressive"
	if t.FieldOrder == "" {
		t.FieldOrder = "progressive"
	}
}
