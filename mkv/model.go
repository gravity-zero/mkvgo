package mkv

import (
	"fmt"
	"time"
)

type TrackType string

const (
	VideoTrack    TrackType = "video"
	AudioTrack    TrackType = "audio"
	SubtitleTrack TrackType = "subtitle"
)

const (
	MsPerHour   = 3600000
	MsPerMinute = 60000
	MsPerSecond = 1000
)

type Container struct {
	Path        string       `json:"path"`
	Info        SegmentInfo  `json:"info"`
	Tracks      []Track      `json:"tracks"`
	Chapters    []Chapter    `json:"chapters"`
	Attachments []Attachment `json:"attachments"`
	Tags        []Tag        `json:"tags"`
	Cues        []CuePoint   `json:"cues,omitempty"`
	DurationMs  int64        `json:"duration_ms"`

	// Keyframes holds the video track's keyframe presentation timestamps in
	// milliseconds (ascending, de-duplicated) — the points an "-c copy" segmenter
	// must cut on. It is filled in the same pass as the rest of the metadata: from
	// the MP4 sync-sample table, or the Matroska Cues seek index. nil when the
	// source carries no usable keyframe index.
	Keyframes []int64 `json:"keyframes,omitempty"`
}

type SegmentInfo struct {
	Title         string     `json:"title"`
	MuxingApp     string     `json:"muxing_app"`
	WritingApp    string     `json:"writing_app"`
	Duration      float64    `json:"duration"`
	TimecodeScale int64      `json:"timecode_scale"`
	DateUTC       *time.Time `json:"date_utc,omitempty"`
	SegmentUID    []byte     `json:"segment_uid,omitempty"`
	PrevUID       []byte     `json:"prev_uid,omitempty"`
	NextUID       []byte     `json:"next_uid,omitempty"`
}

type Track struct {
	ID              uint64    `json:"id"`
	UID             uint64    `json:"uid,omitempty"` // Matroska TrackUID (64-bit); distinct from TrackNumber (ID)
	Type            TrackType `json:"type"`
	Codec           string    `json:"codec"`
	Language        string    `json:"language"` // legacy ISO 639-2 (0x22B59C); "" when absent — see LanguagePresent
	Name            string    `json:"name"`
	IsDefault       bool      `json:"is_default"`
	IsForced        bool      `json:"is_forced"`
	CodecPrivate    []byte    `json:"-"`
	HeaderStripping []byte    `json:"-"` // bytes stripped from each block (ContentCompression)
	Width           *uint32   `json:"width,omitempty"`
	Height          *uint32   `json:"height,omitempty"`
	Channels        *uint8    `json:"channels,omitempty"`
	SampleRate      *float64  `json:"sample_rate,omitempty"`
	BitDepth        *uint8    `json:"bit_depth,omitempty"` // audio bits/sample (0x6264); video uses VideoBitDepth

	// Probe metadata (added v0.4.0). The *Present flags let a consumer tell a
	// value that was explicitly written in the file from a spec default that the
	// reader applied. See ResolvedLanguage for language precedence.
	LanguageBCP47   string `json:"language_bcp47,omitempty"` // raw 0x22B59D, "" if absent
	LanguagePresent bool   `json:"language_present"`         // a Language or LanguageBCP47 element was read
	DefaultPresent  bool   `json:"default_present"`          // a FlagDefault element was read (else IsDefault is the spec default)
	ForcedPresent   bool   `json:"forced_present"`           // a FlagForced element was read

	// Video metadata (added v0.4.0). All nil when the source omits the element.
	// As of v0.6.0 the colour fields and VideoBitDepth are also filled from the
	// codec bitstream (H.264/HEVC SPS VUI, AV1 color_config, VP9 vpcC) when the
	// container Colour element is absent — the container value still wins per field.
	FrameRate     *float64 `json:"frame_rate,omitempty"`      // from DefaultDuration (0x23E383): 1e9/ns
	VideoBitDepth *uint16  `json:"video_bit_depth,omitempty"` // Colour>BitsPerChannel (0x55B2) or SPS bit_depth
	// Colour code points (CICP / ITU-T H.273), nil when absent. Map to the strings
	// ffprobe reports with ColorSpaceName/ColorTransferName/ColorPrimariesName/ColorRangeName.
	ColorSpace     *uint16 `json:"color_space,omitempty"`     // MatrixCoefficients (0x55B1) — ffprobe color_space
	ColorTransfer  *uint16 `json:"color_transfer,omitempty"`  // TransferCharacteristics (0x55BA)
	ColorPrimaries *uint16 `json:"color_primaries,omitempty"` // Primaries (0x55BB)
	ColorRange     *uint16 `json:"color_range,omitempty"`     // Range (0x55B9): 1=tv/limited, 2=pc/full
	Profile        string  `json:"profile,omitempty"`         // codec profile from the SPS, e.g. "Main 10" (v0.6.0)

	// DolbyVision holds the decoded dvcC/dvvC configuration when the track signals
	// Dolby Vision (MP4 dvcC/dvvC box, or Matroska dvcC/dvvC BlockAdditionMapping);
	// nil otherwise. See ParseDolbyVisionConfig.
	DolbyVision *DolbyVision `json:"dolby_vision,omitempty"`
}

// ResolvedLanguage returns the track's effective language per the Matroska spec:
// LanguageBCP47 (0x22B59D) takes precedence over the legacy Language (0x22B59C)
// element when both are present. It returns "" when neither was in the file —
// check LanguagePresent to tell that apart from an explicit empty value.
//
// The two elements use different vocabularies: BCP47 is e.g. "fr" / "pt-BR",
// the legacy element is ISO 639-2 e.g. "fre" / "por". mkvgo returns the raw
// stored value; a consumer normalizing to one scheme must convert.
func (t *Track) ResolvedLanguage() string {
	if t.LanguageBCP47 != "" {
		return t.LanguageBCP47
	}
	return t.Language
}

type Chapter struct {
	ID          uint64    `json:"id"`
	Title       string    `json:"title"`
	StartMs     int64     `json:"start_ms"`
	EndMs       int64     `json:"end_ms"`
	SegmentUID  []byte    `json:"segment_uid,omitempty"`
	SubChapters []Chapter `json:"sub_chapters,omitempty"`
}

type Edition struct {
	Ordered  bool      `json:"ordered"`
	Chapters []Chapter `json:"chapters"`
}

type Attachment struct {
	ID       uint64 `json:"id"`
	Name     string `json:"name"`
	MIMEType string `json:"mime_type"`
	Size     int64  `json:"size"`
	Data     []byte `json:"-"`
}

type Tag struct {
	TargetType string      `json:"target_type"`
	TargetID   uint64      `json:"target_id"`
	SimpleTags []SimpleTag `json:"simple_tags"`
}

type SimpleTag struct {
	Name     string      `json:"name"`
	Value    string      `json:"value,omitempty"`
	Binary   []byte      `json:"binary,omitempty"`
	Language string      `json:"language,omitempty"`
	SubTags  []SimpleTag `json:"sub_tags,omitempty"`
}

type Block struct {
	TrackNumber uint64
	Timecode    int64
	Keyframe    bool
	Data        []byte
	// Duration is the block's duration in milliseconds, read from a BlockGroup's
	// BlockDuration element. It is 0 when absent (e.g. SimpleBlocks, which never
	// carry one). Subtitle cues store their on-screen time here.
	Duration int64
}

// RestoreHeader prepends the stripped header bytes to block data.
func (t *Track) RestoreHeader(data []byte) []byte {
	if len(t.HeaderStripping) == 0 {
		return data
	}
	restored := make([]byte, len(t.HeaderStripping)+len(data))
	copy(restored, t.HeaderStripping)
	copy(restored[len(t.HeaderStripping):], data)
	return restored
}

type CuePoint struct {
	TimeMs     int64
	Track      uint64
	ClusterPos int64
}

type MuxOptions struct {
	OutputPath string
	Tracks     []TrackInput
	Chapters   []Chapter
	Tags       []Tag
}

type DemuxOptions struct {
	SourcePath string
	OutputDir  string
	TrackIDs   []uint64
}

type TrackInput struct {
	SourcePath string
	TrackID    uint64
	Language   string
	Name       string
	IsDefault  bool
}

type MergeInput struct {
	SourcePath string
	TrackIDs   []uint64
}

type MergeOptions struct {
	OutputPath string
	Inputs     []MergeInput
	Progress   ProgressFunc
}

type SplitOptions struct {
	SourcePath string
	OutputDir  string
	Ranges     []TimeRange
	ByChapters bool
	Pattern    string
}

type TimeRange struct {
	StartMs int64
	EndMs   int64
}

// Severity of a validation issue.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

type Issue struct {
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
}

func (i Issue) String() string {
	return fmt.Sprintf("[%s] %s", i.Severity, i.Message)
}

type DiffType string

const (
	DiffAdded   DiffType = "added"
	DiffRemoved DiffType = "removed"
	DiffChanged DiffType = "changed"
)

type Diff struct {
	Type    DiffType `json:"type"`
	Section string   `json:"section"`
	Detail  string   `json:"detail"`
}

func (d Diff) String() string {
	return fmt.Sprintf("[%s] %s: %s", d.Type, d.Section, d.Detail)
}
