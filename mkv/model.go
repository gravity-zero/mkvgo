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

	// Fragmented is true for a fragmented MP4 (an mvex box in the moov), whose
	// per-sample metadata lives in the moof fragments rather than the moov. The
	// probe still recovers the frame rate from the fragment defaults (mvex>trex)
	// and the keyframe index from a random-access index — the mfra/tfra at the
	// file tail, or the sidx Segment Index at the head that streaming fMP4 carries
	// — both bounded and head-only, so a fragmented file usually still reports
	// FrameRate and Keyframes. They stay unset only when no such index is present;
	// a caller should fall back to a full demux when a Fragmented file leaves them
	// empty. Always false for Matroska.
	Fragmented bool `json:"fragmented,omitempty"`
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
	ID               uint64    `json:"id"`
	UID              uint64    `json:"uid,omitempty"` // Matroska TrackUID (64-bit); distinct from TrackNumber (ID)
	Type             TrackType `json:"type"`
	Codec            string    `json:"codec"`
	Language         string    `json:"language"` // legacy ISO 639-2 (0x22B59C); "" when absent — see LanguagePresent
	Name             string    `json:"name"`
	IsDefault        bool      `json:"is_default"`
	IsForced         bool      `json:"is_forced"`
	CodecPrivate     []byte    `json:"-"`
	HeaderStripping  []byte    `json:"-"` // bytes stripped from each block (ContentCompression)
	Width            *uint32   `json:"width,omitempty"`
	Height           *uint32   `json:"height,omitempty"`
	Channels         *uint8    `json:"channels,omitempty"`
	SampleRate       *float64  `json:"sample_rate,omitempty"`        // base/core rate (Matroska SamplingFrequency 0xB5, MP4 AudioSampleEntry)
	OutputSampleRate *float64  `json:"output_sample_rate,omitempty"` // SBR-doubled output rate (Matroska OutputSamplingFrequency 0x78B5, MP4 AAC SBR ext) — see EffectiveSampleRate
	BitDepth         *uint8    `json:"bit_depth,omitempty"`          // audio bits/sample (0x6264); video uses VideoBitDepth

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
	// Rotation is the clockwise display rotation in degrees (0/90/180/270) from the
	// MP4 tkhd display matrix; 0 when none/unknown. Phone videos commonly encode 90
	// or 270 (portrait). ffprobe exposes the same matrix as Display Matrix side data.
	Rotation int `json:"rotation,omitempty"`
	// Display dimensions for anamorphic video, nil when pixels are square. Their
	// ratio is the display aspect: literal pixels from the Matroska
	// DisplayWidth/DisplayHeight (0x54B0/0x54BA), or the reduced (codedW·hSpacing):
	// (codedH·vSpacing) fraction derived from the MP4 pasp box. Read the aspect via
	// DisplayAspectRatio / SampleAspectRatio rather than these raw values.
	DisplayWidth  *uint32 `json:"display_width,omitempty"`
	DisplayHeight *uint32 `json:"display_height,omitempty"`
	// Colour code points (CICP / ITU-T H.273), nil when absent. Map to the strings
	// ffprobe reports with ColorSpaceName/ColorTransferName/ColorPrimariesName/ColorRangeName.
	ColorSpace     *uint16 `json:"color_space,omitempty"`     // MatrixCoefficients (0x55B1) — ffprobe color_space
	ColorTransfer  *uint16 `json:"color_transfer,omitempty"`  // TransferCharacteristics (0x55BA)
	ColorPrimaries *uint16 `json:"color_primaries,omitempty"` // Primaries (0x55BB)
	ColorRange     *uint16 `json:"color_range,omitempty"`     // Range (0x55B9): 1=tv/limited, 2=pc/full
	Profile        string  `json:"profile,omitempty"`         // codec profile from the SPS, e.g. "Main 10" (v0.6.0)
	Level          *uint16 `json:"level,omitempty"`           // codec level_idc from the SPS (ffprobe level): H.264 10×level, HEVC 30×level
	PixelFormat    string  `json:"pixel_format,omitempty"`    // ffprobe pix_fmt (e.g. "yuv420p", "yuv420p10le") from chroma subsampling + bit depth
	FieldOrder     string  `json:"field_order,omitempty"`     // "progressive" or "interlaced" (Matroska FlagInterlaced 0x9A, or H.264 frame_mbs_only_flag); "" unknown
	FrameCount     int64   `json:"frame_count,omitempty"`     // number of frames (ffprobe nb_frames), from the MP4 stsz count; 0 unknown (not head-only for Matroska)
	DurationMs     int64   `json:"duration_ms,omitempty"`     // per-track duration in ms (ffprobe per-stream duration), from the MP4 mdhd; 0 unknown (Matroska has no per-track duration in the header)
	Bitrate        *uint32 `json:"bitrate,omitempty"`         // average stream bitrate in bits/s (MP4 btrt box / esds avgBitrate); nil when unknown

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

// EffectiveSampleRate returns the audio sample rate a decoder produces — the
// value ffprobe reports as sample_rate. For SBR streams (HE-AAC / HE-AACv2) the
// coded core runs at half rate and the decoder doubles it, so OutputSampleRate
// (the Matroska OutputSamplingFrequency, or the AAC AudioSpecificConfig SBR
// extension rate) wins when present; otherwise the base SampleRate. Returns 0
// when neither is known.
func (t *Track) EffectiveSampleRate() float64 {
	if t.OutputSampleRate != nil && *t.OutputSampleRate > 0 {
		return *t.OutputSampleRate
	}
	if t.SampleRate != nil {
		return *t.SampleRate
	}
	return 0
}

// displayDims returns the track's intended display dimensions: the explicit
// DisplayWidth/DisplayHeight when set (anamorphic), else the coded Width/Height
// (square pixels). Both zero when no video dimensions are known.
func (t *Track) displayDims() (uint32, uint32) {
	if t.DisplayWidth != nil && t.DisplayHeight != nil && *t.DisplayWidth > 0 && *t.DisplayHeight > 0 {
		return *t.DisplayWidth, *t.DisplayHeight
	}
	if t.Width != nil && t.Height != nil {
		return *t.Width, *t.Height
	}
	return 0, 0
}

// DisplayAspectRatio returns the picture aspect ratio ffprobe reports as
// display_aspect_ratio ("16:9"), from the display dimensions when the stream is
// anamorphic or the coded dimensions otherwise. "" when no video dimensions are
// known. The fraction is reduced with the same bounded av_reduce ffmpeg uses, so
// a pathological near-square ratio collapses to a sane approximation rather than
// an absurd one (matching ffprobe).
func (t *Track) DisplayAspectRatio() string {
	w, h := t.displayDims()
	if w == 0 || h == 0 {
		return ""
	}
	num, den := avReduce(uint64(w), uint64(h), aspectReduceMax)
	return fmt.Sprintf("%d:%d", num, den)
}

// SampleAspectRatio returns the pixel aspect ratio ffprobe reports as
// sample_aspect_ratio ("1:1" for square pixels, "32:27" for anamorphic). "" when
// the coded dimensions are unknown. Reduced with the same bounded av_reduce as
// DisplayAspectRatio.
//
// On pathological near-square ratios this can differ from ffprobe, which
// re-derives the SAR from the dimension-reduced DAR (so the same VUI SAR prints
// differently at different resolutions). mkvgo reports the exact ratio instead —
// display-only and imperceptible. See CHANGELOG 0.9.0 Notes.
func (t *Track) SampleAspectRatio() string {
	if t.Width == nil || t.Height == nil || *t.Width == 0 || *t.Height == 0 {
		return ""
	}
	dw, dh := t.displayDims()
	// SAR = (DisplayWidth · Height) : (DisplayHeight · Width).
	num := uint64(dw) * uint64(*t.Height)
	den := uint64(dh) * uint64(*t.Width)
	if num == 0 || den == 0 {
		return "1:1"
	}
	rn, rd := avReduce(num, den, aspectReduceMax)
	return fmt.Sprintf("%d:%d", rn, rd)
}

// aspectReduceMax bounds the numerator and denominator of a reduced aspect ratio.
// It is ffmpeg's constant for display aspect ratios (av_reduce(..., 1024*1024)).
const aspectReduceMax = 1024 * 1024

// gcd returns the greatest common divisor of a and b (gcd(x,0)=x).
func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}

// avReduce reduces num/den to lowest terms, then — if either part still exceeds
// max — to the best rational approximation whose numerator and denominator are
// both ≤ max. It is a port of ffmpeg's av_reduce (non-negative inputs), so the
// SAR/DAR strings match ffprobe, including how it tames absurd near-square ratios.
func avReduce(num, den, max uint64) (uint64, uint64) {
	if g := gcd(num, den); g != 0 {
		num, den = num/g, den/g
	}
	a0n, a0d := uint64(0), uint64(1)
	a1n, a1d := uint64(1), uint64(0)
	if num <= max && den <= max {
		a1n, a1d = num, den
		den = 0
	}
	for den != 0 {
		x := num / den
		nextDen := num - den*x
		a2n := x*a1n + a0n
		a2d := x*a1d + a0d
		if a2n > max || a2d > max {
			if a1n != 0 {
				x = (max - a0n) / a1n
			}
			if a1d != 0 {
				if t := (max - a0d) / a1d; t < x {
					x = t
				}
			}
			if den*(2*x*a1d+a0d) > num*a1d {
				a1n, a1d = x*a1n+a0n, x*a1d+a0d
			}
			break
		}
		a0n, a0d = a1n, a1d
		a1n, a1d = a2n, a2d
		num, den = den, nextDen
	}
	if a1d == 0 {
		return a1n, 1
	}
	return a1n, a1d
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
