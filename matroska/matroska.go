// Package matroska provides backward-compatible access to the mkvgo toolkit.
// New code should import mkv, mkv/reader, mkv/writer, mkv/ops, mkv/subtitle directly.
package matroska

import (
	"context"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/ops"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// --- Type aliases ---

type TrackType = mkv.TrackType
type Container = mkv.Container
type SegmentInfo = mkv.SegmentInfo
type Track = mkv.Track
type Chapter = mkv.Chapter
type Attachment = mkv.Attachment
type Tag = mkv.Tag
type SimpleTag = mkv.SimpleTag
type Block = mkv.Block
type CuePoint = mkv.CuePoint
type MuxOptions = mkv.MuxOptions
type DemuxOptions = mkv.DemuxOptions
type TrackInput = mkv.TrackInput
type MergeInput = mkv.MergeInput
type MergeOptions = mkv.MergeOptions
type SplitOptions = mkv.SplitOptions
type TimeRange = mkv.TimeRange
type Severity = mkv.Severity
type Issue = mkv.Issue
type DiffType = mkv.DiffType
type Diff = mkv.Diff
type ProgressFunc = mkv.ProgressFunc
type Options = mkv.Options
type FS = mkv.FS
type BlockReader = reader.BlockReader
type SRTEntry = subtitle.SRTEntry
type ASSFile = subtitle.ASSFile
type ASSEvent = subtitle.ASSEvent

// --- Constants ---

const (
	VideoTrack      = mkv.VideoTrack
	AudioTrack      = mkv.AudioTrack
	SubtitleTrack   = mkv.SubtitleTrack
	SeverityError   = mkv.SeverityError
	SeverityWarning = mkv.SeverityWarning
	DiffAdded       = mkv.DiffAdded
	DiffRemoved     = mkv.DiffRemoved
	DiffChanged     = mkv.DiffChanged
)

var CodecShortName = mkv.CodecShortName

// --- Reader ---

func Open(ctx context.Context, path string) (*Container, error) {
	return reader.Open(ctx, path)
}

func Read(ctx context.Context, r io.ReadSeeker, path string) (*Container, error) {
	return reader.Read(ctx, r, path)
}

func NewBlockReader(r io.ReadSeeker, timecodeScale int64) (*BlockReader, error) {
	return reader.NewBlockReader(r, timecodeScale)
}

// --- Writer ---

func Write(w io.Writer, c *Container) error {
	return writer.Write(w, c)
}

// --- Operations ---

func Mux(ctx context.Context, opts MuxOptions) error                 { return ops.Mux(ctx, opts) }
func Demux(ctx context.Context, opts DemuxOptions) error             { return ops.Demux(ctx, opts) }
func Split(ctx context.Context, opts SplitOptions) ([]string, error) { return ops.Split(ctx, opts) }
func Join(ctx context.Context, sources []string, dstPath string) error {
	return ops.Join(ctx, sources, dstPath)
}
func Merge(ctx context.Context, opts MergeOptions) error         { return ops.Merge(ctx, opts) }
func Validate(ctx context.Context, path string) ([]Issue, error) { return ops.Validate(ctx, path) }
func Compare(ctx context.Context, pathA, pathB string) ([]Diff, error) {
	return ops.Compare(ctx, pathA, pathB)
}

func RemoveTrack(ctx context.Context, srcPath, dstPath string, removeIDs []uint64, opts ...Options) error {
	return ops.RemoveTrack(ctx, srcPath, dstPath, removeIDs, opts...)
}

func AddTrack(ctx context.Context, srcPath, dstPath string, input TrackInput, opts ...Options) error {
	return ops.AddTrack(ctx, srcPath, dstPath, input, opts...)
}

func EditMetadata(ctx context.Context, srcPath, dstPath string, edit func(*Container), opts ...Options) error {
	return ops.EditMetadata(ctx, srcPath, dstPath, edit, opts...)
}

func EditInPlace(ctx context.Context, path string, edit func(*Container), opts ...Options) error {
	return ops.EditInPlace(ctx, path, edit, opts...)
}

func ExtractAttachment(ctx context.Context, srcPath string, attachID uint64, outPath string, opts ...Options) error {
	return ops.ExtractAttachment(ctx, srcPath, attachID, outPath, opts...)
}

func ExtractSubtitle(ctx context.Context, srcPath string, trackID uint64, outPath string, opts ...Options) error {
	return ops.ExtractSubtitle(ctx, srcPath, trackID, outPath, opts...)
}

func ExtractASS(ctx context.Context, srcPath string, trackID uint64, outPath string, opts ...Options) error {
	return ops.ExtractASS(ctx, srcPath, trackID, outPath, opts...)
}

func MergeSubtitle(ctx context.Context, srcPath, srtPath, dstPath string, lang, name string, opts ...Options) error {
	return ops.MergeSubtitle(ctx, srcPath, srtPath, dstPath, lang, name, opts...)
}

func MergeASS(ctx context.Context, srcPath, assPath, dstPath string, lang, name string, opts ...Options) error {
	return ops.MergeASS(ctx, srcPath, assPath, dstPath, lang, name, opts...)
}

func MergeWithSubtitles(ctx context.Context, basePath, srtPath, dstPath string, srtLang, srtName string, extraSources []MergeInput, opts ...Options) error {
	return ops.MergeWithSubtitles(ctx, basePath, srtPath, dstPath, srtLang, srtName, extraSources, opts...)
}

// --- Subtitle parsers ---

var (
	ParseSRT           = subtitle.ParseSRT
	ParseASS           = subtitle.ParseASS
	FormatASSTimestamp = subtitle.FormatASSTimestamp
)
