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
	IDTrackEntry   = 0xAE
	IDTrackNumber  = 0xD7
	IDTrackUID     = 0x73C5
	IDTrackType    = 0x83
	IDFlagDefault  = 0x88
	IDFlagForced   = 0x55AA
	IDCodecID      = 0x86
	IDLanguage     = 0x22B59C
	IDName         = 0x536E
	IDCodecPrivate = 0x63A2
)

const (
	IDContentEncodings    = 0x6D80
	IDContentEncoding     = 0x6240
	IDContentCompression  = 0x5034
	IDContentCompAlgo     = 0x4254
	IDContentCompSettings = 0x4255
)

const (
	IDVideo       = 0xE0
	IDPixelWidth  = 0xB0
	IDPixelHeight = 0xBA
)

const (
	IDAudio        = 0xE1
	IDSamplingFreq = 0xB5
	IDChannels     = 0x9F
	IDBitDepth     = 0x6264
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
	"S_TEXT/ASS":       "ass",
	"S_TEXT/SSA":       "ssa",
	"S_VOBSUB":         "vobsub",
	"S_HDMV/PGS":       "pgs",
	"S_DVBSUB":         "dvbsub",
}
