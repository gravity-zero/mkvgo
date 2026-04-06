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
	Type            TrackType `json:"type"`
	Codec           string    `json:"codec"`
	Language        string    `json:"language"`
	Name            string    `json:"name"`
	IsDefault       bool      `json:"is_default"`
	IsForced        bool      `json:"is_forced"`
	CodecPrivate    []byte    `json:"-"`
	HeaderStripping []byte    `json:"-"` // bytes stripped from each block (ContentCompression)
	Width           *uint32   `json:"width,omitempty"`
	Height          *uint32   `json:"height,omitempty"`
	Channels        *uint8    `json:"channels,omitempty"`
	SampleRate      *float64  `json:"sample_rate,omitempty"`
	BitDepth        *uint8    `json:"bit_depth,omitempty"`
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
