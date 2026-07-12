package mkv

import "fmt"

// codec_names.go - display-only derivations that probers report but the mkvgo
// fast path otherwise omits: codec_long_name and channel_layout. Both are pure
// functions of fields already read (Codec, Channels), exposed as methods like the
// colour name helpers so a consumer can match standard prober strings.

// codecLongNames maps an mkvgo short codec name (Track.Codec) to the codec_long_name
// probers print as codec_long_name. Only codecs mkvgo recognises are
// listed; anything absent yields "".
var codecLongNames = map[string]string{
	"h264":      "H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10",
	"hevc":      "H.265 / HEVC (High Efficiency Video Coding)",
	"av1":       "Alliance for Open Media AV1",
	"vp8":       "On2 VP8",
	"vp9":       "Google VP9",
	"aac":       "AAC (Advanced Audio Coding)",
	"ac3":       "ATSC A/52A (AC-3)",
	"eac3":      "ATSC A/52B (AC-3, E-AC-3)",
	"dts":       "DCA (DTS Coherent Acoustics)",
	"flac":      "FLAC (Free Lossless Audio Codec)",
	"opus":      "Opus (Opus Interactive Audio Codec)",
	"vorbis":    "Vorbis",
	"truehd":    "TrueHD",
	"A_MPEG/L3": "MP3 (MPEG audio layer 3)",
	"srt":       "SubRip subtitle",
	"webvtt":    "WebVTT subtitle",
	"ass":       "ASS (Advanced SubStation Alpha) subtitle",
	"ssa":       "SSA (SubStation Alpha) subtitle",
	"vobsub":    "DVD subtitles",
	"pgs":       "HDMV Presentation Graphic Stream subtitles",
	"dvbsub":    "DVB subtitles",
}

// CodecLongName returns the conventional codec_long_name for the track's codec, or ""
// when the codec is not mapped. It is a static lookup on Track.Codec.
func (t *Track) CodecLongName() string { return codecLongNames[t.Codec] }

// ChannelLayout returns the audio channel layout probers report as channel_layout
// ("mono", "stereo", "5.1(side)", "7.1", …), derived from the channel count. "" when
// the track is not audio or the count is unknown. The surround variant depends on
// the codec's decoded channel positions, which is not fully recoverable head-only:
// most codecs (AAC, E-AC-3, DTS) place 5.1 surrounds at the sides → "5.1(side)",
// while AC-3 uses the back positions → "5.1". Uncommon counts fall back to the conventional
// "N channels" form.
func (t *Track) ChannelLayout() string {
	if t.Channels == nil || *t.Channels == 0 {
		return ""
	}
	switch *t.Channels {
	case 1:
		return "mono"
	case 2:
		return "stereo"
	case 3:
		return "3.0"
	case 4:
		return "4.0"
	case 5:
		return "5.0(side)"
	case 6:
		if t.Codec == "ac3" {
			return "5.1"
		}
		return "5.1(side)"
	case 7:
		return "6.1"
	case 8:
		return "7.1"
	}
	return fmt.Sprintf("%d channels", *t.Channels)
}
