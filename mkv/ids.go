package mkv

const IDVoid = 0xEC

const (
	IDSegment     = 0x18538067
	IDSeekHead    = 0x114D9B74
	IDInfo        = 0x1549A966
	IDTracks      = 0x1654AE6B
	IDCues        = 0x1C53BB6B
	IDAttachments = 0x1941A469
	IDChapters    = 0x1043A770
	IDTags        = 0x1254C367
	IDCluster     = 0x1F43B675
)

const (
	IDSegmentUID = 0x73A4
	IDPrevUID    = 0x3CB923
	IDNextUID    = 0x3EB923
)

const (
	IDTimecodeScale = 0x2AD7B1
	IDDuration      = 0x4489
	IDMuxingApp     = 0x4D80
	IDWritingApp    = 0x5741
	IDDateUTC       = 0x4461
	IDTitle         = 0x7BA9
)

const (
	IDTrackEntry           = 0xAE
	IDTrackNumber          = 0xD7
	IDTrackUID             = 0x73C5
	IDTrackType            = 0x83
	IDFlagDefault          = 0x88
	IDFlagForced           = 0x55AA
	IDFlagHearingImpaired  = 0x55AB // the conventional disposition field hearing_impaired
	IDFlagVisualImpaired   = 0x55AC // the conventional disposition field visual_impaired
	IDFlagTextDescriptions = 0x55AD // the conventional disposition field descriptions
	IDFlagOriginal         = 0x55AE // the conventional disposition field original
	IDFlagCommentary       = 0x55AF // the conventional disposition field comment
	IDCodecID              = 0x86
	IDLanguage             = 0x22B59C // legacy ISO 639-2 language element
	IDLanguageBCP47        = 0x22B59D // IETF BCP-47 language element (takes precedence)
	IDName                 = 0x536E
	IDCodecPrivate         = 0x63A2
	IDDefaultDuration      = 0x23E383 // nominal frame duration in ns (fps = 1e9/value)
	IDCodecDelay           = 0x56AA   // codec built-in delay in ns (gapless/encoder priming)
	IDSeekPreRoll          = 0x56BB   // pre-roll to decode before a seek point, in ns
)

const (
	IDContentEncodings    = 0x6D80
	IDContentEncoding     = 0x6240
	IDContentCompression  = 0x5034
	IDContentCompAlgo     = 0x4254
	IDContentCompSettings = 0x4255
)

const (
	IDVideo          = 0xE0
	IDPixelWidth     = 0xB0
	IDPixelHeight    = 0xBA
	IDDisplayWidth   = 0x54B0
	IDDisplayHeight  = 0x54BA
	IDFlagInterlaced = 0x9A
	IDStereoMode     = 0x53B8 // 3D stereo arrangement (0 = mono)
	IDProjection     = 0x7670 // video projection (360/spherical) container
	IDProjectionType = 0x7671 // 0 rectangular, 1 equirectangular, 2 cubemap, 3 mesh
)

// BlockAdditionMapping (TrackEntry sub-element) and its children. It carries
// codec-private supplemental data keyed by a four-character BlockAddIDType; for
// Dolby Vision the type is dvcC/dvvC and the ExtraData is the
// DOVIDecoderConfigurationRecord. See mkv/dolbyvision.go.
const (
	IDBlockAdditionMapping = 0x41E4
	IDBlockAddIDValue      = 0x41F0
	IDBlockAddIDName       = 0x41A4
	IDBlockAddIDType       = 0x41E7
	IDBlockAddIDExtraData  = 0x41ED

	// BlockAddIDType values for Dolby Vision (the four-character codes as uint32).
	BlockAddIDTypeDVCC = 0x64766343 // "dvcC"
	BlockAddIDTypeDVVC = 0x64767643 // "dvvC"
)

// Colour element (0x55B0) and its sub-elements. Values are CICP / ITU-T H.273
// code points - the integers mainstream probers expose as color_space,
// color_transfer, color_primaries and color_range. See mkv/colour.go for the
// code-point → conventional-name mapping.
const (
	IDColour               = 0x55B0
	IDColourMatrix         = 0x55B1 // MatrixCoefficients  → conventional color_space
	IDColourBitsPerChannel = 0x55B2 // BitsPerChannel      → video bit depth
	IDColourRange          = 0x55B9 // Range               → conventional color_range
	IDColourTransfer       = 0x55BA // TransferCharacter.  → conventional color_transfer
	IDColourPrimaries      = 0x55BB // Primaries           → conventional color_primaries
	IDColourMaxCLL         = 0x55BC // MaxCLL (cd/m²)      → HDR10 content light level
	IDColourMaxFALL        = 0x55BD // MaxFALL (cd/m²)     → HDR10 frame-average light
)

// MasteringMetadata (0x55D0) and its sub-elements (SMPTE ST 2086 mastering display
// colour volume), nested inside the Colour element. Chromaticities are EBML floats
// in CIE 1931 (x,y) 0..1; luminances are EBML floats in cd/m².
const (
	IDMasteringMetadata = 0x55D0
	IDPrimaryRChromaX   = 0x55D1
	IDPrimaryRChromaY   = 0x55D2
	IDPrimaryGChromaX   = 0x55D3
	IDPrimaryGChromaY   = 0x55D4
	IDPrimaryBChromaX   = 0x55D5
	IDPrimaryBChromaY   = 0x55D6
	IDWhitePointChromaX = 0x55D7
	IDWhitePointChromaY = 0x55D8
	IDLuminanceMax      = 0x55D9
	IDLuminanceMin      = 0x55DA
)

const (
	IDAudio              = 0xE1
	IDSamplingFreq       = 0xB5
	IDOutputSamplingFreq = 0x78B5
	IDChannels           = 0x9F
	IDBitDepth           = 0x6264
)

const (
	IDEditionEntry       = 0x45B9
	IDChapterAtom        = 0xB6
	IDChapterUID         = 0x73C4
	IDChapterTimeStart   = 0x91
	IDChapterTimeEnd     = 0x92
	IDChapterDisplay     = 0x80
	IDChapString         = 0x85
	IDChapterSegmentUID  = 0x6E67
	IDEditionFlagOrdered = 0x45DD
)

const (
	IDAttachedFile    = 0x61A7
	IDFileDescription = 0x467E
	IDFileName        = 0x466E
	IDFileMimeType    = 0x4660
	IDFileData        = 0x465C
	IDFileUID         = 0x46AE
)

const (
	IDTag             = 0x7373
	IDTargets         = 0x63C0
	IDTargetTypeValue = 0x68CA
	IDTargetType      = 0x63CA
	IDTagTrackUID     = 0x63C5
	IDSimpleTag       = 0x67C8
	IDTagName         = 0x45A3
	IDTagString       = 0x4487
	IDTagLanguage     = 0x447A
	IDTagBinary       = 0x4485
)

const (
	IDTimestamp      = 0xE7
	IDSimpleBlock    = 0xA3
	IDBlockGroup     = 0xA0
	IDBlock          = 0xA1
	IDBlockDuration  = 0x9B
	IDReferenceBlock = 0xFB
)

const (
	IDEditionUID         = 0x45BC
	IDEditionFlagHidden  = 0x45BD
	IDEditionFlagDefault = 0x45DB
	IDChapterFlagHidden  = 0x98
	IDChapterFlagEnabled = 0x4598
	IDChapLanguage       = 0x437C
)

const (
	IDCuePoint          = 0xBB
	IDCueTime           = 0xB3
	IDCueTrackPositions = 0xB7
	IDCueTrack          = 0xF7
	IDCueClusterPos     = 0xF1
)

const (
	IDSeek         = 0x4DBB
	IDSeekID       = 0x53AB
	IDSeekPosition = 0x53AC
)

const (
	TrackTypeVideo    = 1
	TrackTypeAudio    = 2
	TrackTypeSubtitle = 17
)

var CodecShortName = map[string]string{
	"V_MPEG4/ISO/AVC":  "h264",
	"V_MPEGH/ISO/HEVC": "hevc",
	"V_VP8":            "vp8",
	"V_VP9":            "vp9",
	"V_AV1":            "av1",
	"V_MS/VFW/FOURCC":  "vfw",
	"A_AAC":            "aac",
	"A_AC3":            "ac3",
	"A_EAC3":           "eac3",
	"A_DTS":            "dts",
	"A_FLAC":           "flac",
	"A_VORBIS":         "vorbis",
	"A_OPUS":           "opus",
	"A_TRUEHD":         "truehd",
	"A_PCM/INT/LIT":    "pcm",
	"S_TEXT/UTF8":      "srt",
	"S_TEXT/WEBVTT":    "webvtt",
	"S_TEXT/ASS":       "ass",
	"S_TEXT/SSA":       "ssa",
	"S_VOBSUB":         "vobsub",
	"S_HDMV/PGS":       "pgs",
	"S_DVBSUB":         "dvbsub",
}

// ffprobeCodecName maps the (intentionally kept) mkvgo short codec names to the
// codec_name external probers report for the same stream, for the cases where
// the two diverge. mkvgo does NOT rename its own values (existing consumers rely
// on them); this lookup lets a consumer normalize to that vocabulary when it
// wants probe equivalence. Only divergent names are listed - anything absent is
// identical in both tools (e.g. h264, hevc, aac, opus, ass).
var ffprobeCodecName = map[string]string{
	"srt":    "subrip",            // S_TEXT/UTF8
	"vobsub": "dvd_subtitle",      // S_VOBSUB
	"pgs":    "hdmv_pgs_subtitle", // S_HDMV/PGS
	"dvbsub": "dvb_subtitle",      // S_DVBSUB
}

// FFprobeCodecName returns the codec_name an external prober reports for a track whose
// mkvgo Codec is shortName (as found in Track.Codec / CodecShortName). For codecs
// that share the same name in both tools it returns shortName unchanged, so it is
// always safe to call. It does not mutate Track.Codec.
func FFprobeCodecName(shortName string) string {
	if n, ok := ffprobeCodecName[shortName]; ok {
		return n
	}
	return shortName
}
