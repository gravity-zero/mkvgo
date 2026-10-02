package mkv

import "strings"

// ffprobeCodecNameByID maps the Matroska CodecIDs that carry a "/" and name
// ONE codec to the codec_name external probers report. They are resolved by
// Track.FFprobeCodecName only: FFprobeCodecName(string) keeps returning them
// unchanged, the "/" being its mark of a name it did not resolve.
var ffprobeCodecNameByID = map[string]string{
	"V_MPEG4/ISO/SP":     "mpeg4",
	"V_MPEG4/ISO/ASP":    "mpeg4",
	"V_MPEG4/ISO/AP":     "mpeg4",
	"V_MPEG4/MS/V3":      "msmpeg4v3",
	"V_REAL/RV40":        "rv40",
	"A_MPEG/L1":          "mp1",
	"A_MPEG/L2":          "mp2",
	"A_MPEG/L3":          "mp3",
	"A_REAL/14_4":        "ra_144",
	"A_REAL/SIPR":        "sipr",
	"A_ATRAC/AT1":        "atrac1",
	"A_DTS/EXPRESS":      "dts",
	"A_DTS/LOSSLESS":     "dts",
	"A_AC3/BSID9":        "ac3",
	"A_AC3/BSID10":       "ac3",
	"A_AAC/MPEG2/MAIN":   "aac",
	"A_AAC/MPEG2/LC":     "aac",
	"A_AAC/MPEG2/LC/SBR": "aac",
	"A_AAC/MPEG2/SSR":    "aac",
	"A_AAC/MPEG4/MAIN":   "aac",
	"A_AAC/MPEG4/LC":     "aac",
	"A_AAC/MPEG4/LC/SBR": "aac",
	"A_AAC/MPEG4/SSR":    "aac",
	"A_AAC/MPEG4/LTP":    "aac",
	"S_TEXT/ASCII":       "text",
	"S_HDMV/TEXTST":      "hdmv_text_subtitle",
}

// ffprobeCodecNameByFourCC maps the FourCC of a V_MS/VFW/FOURCC track (upper
// case) to the prober's codec_name.
var ffprobeCodecNameByFourCC = map[string]string{
	"XVID": "mpeg4", "DIVX": "mpeg4", "DX50": "mpeg4", "FMP4": "mpeg4", "MP4V": "mpeg4", "3IV2": "mpeg4",
	"MP41": "msmpeg4v1", "MPG4": "msmpeg4v1", "MP42": "msmpeg4v2",
	"MP43": "msmpeg4v3", "DIV3": "msmpeg4v3", "DIV4": "msmpeg4v3",
	"MJPG": "mjpeg", "WMV1": "wmv1", "WMV2": "wmv2",
	"MPG1": "mpeg1video", "MPG2": "mpeg2video",
	"VP80": "vp8", "VP90": "vp9", "AV01": "av1", "VP60": "vp6", "VP62": "vp6",
	"FFV1": "ffv1", "MSVC": "msvideo1", "CRAM": "msvideo1", "DVSD": "dvvideo",
	"IV50": "indeo5", "CVID": "cinepak", "SVQ3": "svq3", "H263": "h263",
	"I420": "rawvideo", "YV12": "rawvideo",
}

// vfwCodecID and the PCM CodecIDs name a family: which codec a track holds
// depends on its parameters.
const (
	vfwCodecID      = "V_MS/VFW/FOURCC"
	pcmIntLitID     = "A_PCM/INT/LIT"
	pcmIntBigID     = "A_PCM/INT/BIG"
	pcmFloatIEEEID  = "A_PCM/FLOAT/IEEE"
	vfwFourCCOffset = 16 // biCompression in the BITMAPINFOHEADER a VFW track's CodecPrivate holds
)

// FFprobeCodecName returns the codec_name an external prober reports for the
// track. Unlike FFprobeCodecName(shortName) it has the whole track to go by,
// so it also resolves the names that depend on more than the CodecID: a
// V_MS/VFW/FOURCC track by the FourCC in its CodecPrivate (XVID is mpeg4), a
// PCM track by its bit depth (pcm_s24le), and the CodecIDs carrying a "/".
//
// What it cannot resolve - an unknown FourCC, a PCM track stating no bit
// depth, a CodecID outside its tables - is returned as the raw Matroska
// CodecID, never as a guess: a name starting with "V_", "A_" or "S_" is one a
// caller should confirm by another means.
func (t Track) FFprobeCodecName() string {
	switch t.Codec {
	case "vfw", vfwCodecID:
		if len(t.CodecPrivate) >= vfwFourCCOffset+4 {
			if n, ok := ffprobeCodecNameByFourCC[strings.ToUpper(string(t.CodecPrivate[vfwFourCCOffset:vfwFourCCOffset+4]))]; ok {
				return n
			}
		}
		return vfwCodecID
	case "pcm", pcmIntLitID:
		return pcmCodecName(t.BitDepth, pcmIntLitID, "pcm_u8", "pcm_s16le", "pcm_s24le", "pcm_s32le")
	case pcmIntBigID:
		return pcmCodecName(t.BitDepth, pcmIntBigID, "pcm_u8", "pcm_s16be", "pcm_s24be", "pcm_s32be")
	case pcmFloatIEEEID:
		switch {
		case t.BitDepth == nil:
			return pcmFloatIEEEID
		case *t.BitDepth == 64:
			return "pcm_f64le"
		case *t.BitDepth == 32:
			return "pcm_f32le"
		}
		return pcmFloatIEEEID
	}
	if n, ok := ffprobeCodecNameByID[t.Codec]; ok {
		return n
	}
	return FFprobeCodecName(t.Codec)
}

// pcmCodecName picks an integer PCM codec_name by bit depth; rawID when the
// track states none, or one outside the usual four.
func pcmCodecName(depth *uint8, rawID, u8, s16, s24, s32 string) string {
	if depth == nil {
		return rawID
	}
	switch *depth {
	case 8:
		return u8
	case 16:
		return s16
	case 24:
		return s24
	case 32:
		return s32
	}
	return rawID
}
