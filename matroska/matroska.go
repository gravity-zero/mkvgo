// Package matroska is the stable, supported public API of mkvgo: a small,
// curated facade over the lower-level building blocks in mkv and its
// subpackages. Prefer it for application code - its exported surface is the one
// kept backward-compatible.
//
// The mkv, mkv/reader, mkv/writer, mkv/ops and mkv/subtitle packages are
// lower-level and EXPERIMENTAL: their APIs may change between minor versions.
// Import them directly only for capabilities this facade does not expose yet
// (e.g. streaming readers/writers, NewWebMStreamWriter).
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

// ErrNotMatroska is returned by Open/Read/OpenMeta/ReadMeta when the input is not
// Matroska/WebM but looks like an MP4-family (ISO base media) file. Catch it with
// errors.Is to re-route to the mp4 reader.
var ErrNotMatroska = reader.ErrNotMatroska

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

// FFprobeCodecName maps an mkvgo short codec name to the conventional codec_name
// probers report, for consumers normalizing track metadata to that vocabulary.
var FFprobeCodecName = mkv.FFprobeCodecName

// --- Reader ---

// Open reads the FULL metadata of the Matroska/WebM file at path: Info, Tracks,
// Chapters, Attachments (with data), Tags and the Cues seek index, walking the
// whole Segment (Clusters are skipped by seeking, block payloads are not read).
// For a cheap head-only read (streaming indexers, probes), use OpenMeta instead.
// Returns ErrNotMatroska when the file is not an EBML/Matroska container.
func Open(ctx context.Context, path string) (*Container, error) {
	return reader.Open(ctx, path)
}

// Read is Open over a caller-provided io.ReadSeeker (a seekable source is
// required: the parser follows SeekHead and skips Clusters by seeking; for a
// pure forward-only io.Reader use reader.ReadStream). path is informational
// (Container.Path, error messages).
func Read(ctx context.Context, r io.ReadSeeker, path string) (*Container, error) {
	return reader.Read(ctx, r, path)
}

// OpenMeta is the fast metadata-only counterpart of Open: it returns Tracks +
// Info (and DurationMs) but stops as soon as it has them, never parsing Cues or
// traversing Clusters. Chapters/Attachments/Tags/Cues are left nil. Use it for
// library indexing where only stream metadata is needed.
func OpenMeta(ctx context.Context, path string, opts ...ReadOption) (*Container, error) {
	return reader.OpenMeta(ctx, path, opts...)
}

// OpenMetaWithFS is OpenMeta against a caller-provided FS.
func OpenMetaWithFS(ctx context.Context, path string, fs *FS, opts ...ReadOption) (*Container, error) {
	return reader.OpenMetaWithFS(ctx, path, fs, opts...)
}

// ReadMeta is the fast metadata-only counterpart of Read: Tracks + Info (and
// DurationMs) only, with Chapters/Attachments/Tags/Cues left nil. See OpenMeta.
func ReadMeta(ctx context.Context, r io.ReadSeeker, path string, opts ...ReadOption) (*Container, error) {
	return reader.ReadMeta(ctx, r, path, opts...)
}

// KeyframeSample is one extracted video keyframe, decoder-ready. See
// ExtractKeyframeSample.
type KeyframeSample = ops.KeyframeSample

// ExtractKeyframeSample returns the video keyframe nearest atMs, seeked
// through the Cues (a few bounded reads) and packed for a decoder: Annex-B
// with parameter sets for H.264/HEVC, an IVF wrapper for VP8/VP9/AV1. The
// building block of thumbnail/storyboard pipelines - decoding the image is
// the consumer's job (any decoder takes it as-is).
func ExtractKeyframeSample(ctx context.Context, srcPath string, atMs int64, opts ...Options) (*KeyframeSample, error) {
	return ops.ExtractKeyframeSample(ctx, srcPath, atMs, opts...)
}

// --- Playability / ABR ladder ---

// Target, TrackVerdict and PlayabilityReport are the playability decision
// model - see ops.Target for the capability fields and TargetByName for the
// built-in profiles.
type Target = ops.Target
type TrackVerdict = ops.TrackVerdict
type PlayabilityReport = ops.PlayabilityReport

// Playability evaluates every track of the file at path against target
// (head-only metadata only - no decode) and returns the per-track and
// overall direct-play/remux/transcode verdicts. See ops.Playability.
func Playability(ctx context.Context, path string, target Target, opts ...Options) (*PlayabilityReport, error) {
	return ops.Playability(ctx, path, target, opts...)
}

// TargetByName returns one of the built-in playability capability profiles
// ("safari", "chrome", "firefox", "chromecast-gen3", "mse-generic",
// "chromium-generic", "brave", "opera", "vivaldi", "samsung-internet",
// "edge"), or (Target{}, false) for an unknown name.
func TargetByName(name string) (Target, bool) {
	return ops.TargetByName(name)
}

// Rung and LadderInput are the ABR ladder recommendation model - see
// ops.RecommendLadder.
type Rung = ops.Rung
type LadderInput = ops.LadderInput

// RecommendLadder returns a sensible ABR ladder capped at the source
// resolution/bitrate: guidance, not a guarantee - mkvgo never transcodes.
// See ops.RecommendLadder.
func RecommendLadder(in LadderInput) []Rung {
	return ops.RecommendLadder(in)
}

// RecommendLadderFor derives a LadderInput from a file's video track
// (head-only metadata) and returns RecommendLadder's result.
func RecommendLadderFor(ctx context.Context, path string, opts ...Options) ([]Rung, error) {
	return ops.RecommendLadderFor(ctx, path, opts...)
}

// --- Ingest ---

// ServingStrategy is Ingest's top-level serving recommendation. See
// ops.ServingStrategy.
type ServingStrategy = ops.ServingStrategy

const (
	StrategyDirectPlay = ops.StrategyDirectPlay
	StrategyRemuxHLS   = ops.StrategyRemuxHLS
	StrategyTranscode  = ops.StrategyTranscode
)

// ServingPlan is Ingest's result. See ops.ServingPlan.
type ServingPlan = ops.ServingPlan

// IngestOptions configures Ingest. See ops.IngestOptions.
type IngestOptions = ops.IngestOptions

// Ingest composes Playability, RecommendLadderFor and (optionally)
// ReindexInPlace into a single per-file "serving plan" decision for a media
// server's onboarding: whether to direct-play the source, remux it to
// on-demand HLS (patching in a seek index first when opts.Reindex is set and
// none exists), or recommend an ABR ladder for an external transcode. Ingest
// itself never decodes or transcodes anything. See ops.Ingest.
func Ingest(ctx context.Context, path string, opts IngestOptions) (*ServingPlan, error) {
	return ops.Ingest(ctx, path, opts)
}

// ReadOption configures an optional metadata-read behaviour (see
// WithInBandColourFallback).
type ReadOption = reader.ReadOption

// WithInBandColourFallback recovers a video track's colour from the first
// sample's in-band SPS when it is absent from both the container and the codec-
// private record (a bare hvcC). Off by default; the read stays head-only unless
// passed. See reader.WithInBandColourFallback.
func WithInBandColourFallback() ReadOption { return reader.WithInBandColourFallback() }

// WithSampledKeyframes enables a bounded, coarse keyframe index for a Matroska
// that carries no Cues: it samples Cluster timestamps at n evenly-spaced byte
// offsets (n ≤ 0 uses a default), so a Cues-less file still reports Keyframes
// instead of leaving the caller to an external probe. Files with Cues never
// sample. See reader.WithSampledKeyframes.
func WithSampledKeyframes(n int) ReadOption { return reader.WithSampledKeyframes(n) }

// WithKeyframeIndex builds a COMPLETE video keyframe index (every keyframe, equal
// keyframe, from the block headers) for a Cues-less Matroska, via one sequential
// header-only pass over the Segment - no demux, no decode. Use it for the "no
// external fallback" path; prefer WithSampledKeyframes for a cheaper coarse
// index. Files with Cues are never scanned. See reader.WithKeyframeIndex.
func WithKeyframeIndex() ReadOption { return reader.WithKeyframeIndex() }

// WithBitrate fills each track's Bitrate from the Matroska "BPS" tag (the per-track
// bitrate probers show as TAG:BPS; their own bit_rate field stays N/A for
// Matroska) on the metadata-only path, which otherwise leaves Bitrate nil for
// Matroska. It follows the head SeekHead straight to the Tags element (one seek, no
// Cluster scan); the raw Tags stay nil. No effect on MP4. See reader.WithBitrate.
func WithBitrate() ReadOption { return reader.WithBitrate() }

func NewBlockReader(r io.ReadSeeker, timecodeScale int64) (*BlockReader, error) {
	return reader.NewBlockReader(r, timecodeScale)
}

// TrackDefaultDurations builds the BlockReader.SetTrackDefaultDurations
// argument (track number → DefaultDuration ns) from parsed track metadata. A
// laced block stores one timecode for its N frames; with the stride set the
// reader delivers frame i at blockTS + i×duration. A sequential BlockReader
// picks the durations up on its own when it walks over the Tracks element;
// set them explicitly when reading from a mid-file offset.
func TrackDefaultDurations(tracks []mkv.Track) map[uint64]int64 {
	return reader.TrackDefaultDurations(tracks)
}

// --- Writer ---

// Write serialises c's METADATA ONLY - EBML header, Info, Tracks, Chapters,
// Attachments and Tags. It writes NO Clusters (a Container holds no block
// data) and no Cues/SeekHead, so the result is not a playable media file.
// To produce a complete file, remux from a source (RemuxToWebM, mp4.RemuxToMP4,
// Mux/Merge) or write blocks yourself via writer.MKVWriter / writer.NewStreamWriter.
func Write(w io.Writer, c *Container) error {
	return writer.Write(w, c)
}

// WriteWebM writes c as a WebM file (validates WebM codec compatibility, then
// writes with the "webm" DocType). See mkv.ValidateWebM.
func WriteWebM(w io.Writer, c *Container) error {
	return writer.WriteWebM(w, c)
}

// ValidateWebM reports whether c can be written as WebM, naming any track whose
// codec falls outside the WebM subset (VP8/VP9/AV1, Vorbis/Opus, WebVTT).
func ValidateWebM(c *Container) error {
	return mkv.ValidateWebM(c)
}

// IsWebMCodec reports whether a codec (short name "vp9" or Matroska id "V_VP9")
// is permitted in WebM.
func IsWebMCodec(codec string) bool {
	return mkv.IsWebMCodec(codec)
}

// WebMDocTypeVersion returns the EBML DocTypeVersion needed for c as WebM
// (4 when an AV1 track is present, else 2).
func WebMDocTypeVersion(c *Container) uint64 {
	return mkv.WebMDocTypeVersion(c)
}

// RemuxToWebM reads srcPath and writes a complete, playable WebM file to
// dstPath, copying the media verbatim. Rejects sources with non-WebM codecs.
// Non-subset elements (Chapters/Attachments/Tags) are dropped - see
// WebMNonSubsetElements to detect that loss beforehand.
func RemuxToWebM(ctx context.Context, srcPath, dstPath string, opts ...Options) error {
	return ops.RemuxToWebM(ctx, srcPath, dstPath, opts...)
}

// WebMNonSubsetElements lists the elements in c (Chapters/Attachments/Tags) that
// a WebM remux will drop; empty means nothing is lost.
func WebMNonSubsetElements(c *Container) []string {
	return mkv.WebMNonSubsetElements(c)
}

// --- Operations ---

func Mux(ctx context.Context, opts MuxOptions, extra ...Options) error {
	return ops.Mux(ctx, opts, extra...)
}
func Demux(ctx context.Context, opts DemuxOptions, extra ...Options) error {
	return ops.Demux(ctx, opts, extra...)
}
func Split(ctx context.Context, opts SplitOptions, extra ...Options) ([]string, error) {
	return ops.Split(ctx, opts, extra...)
}
func Join(ctx context.Context, sources []string, dstPath string, opts ...Options) error {
	return ops.Join(ctx, sources, dstPath, opts...)
}
func Merge(ctx context.Context, opts MergeOptions, extra ...Options) error {
	return ops.Merge(ctx, opts, extra...)
}
func Validate(ctx context.Context, path string, opts ...Options) ([]Issue, error) {
	return ops.Validate(ctx, path, opts...)
}
func Compare(ctx context.Context, pathA, pathB string, opts ...Options) ([]Diff, error) {
	return ops.Compare(ctx, pathA, pathB, opts...)
}

// TrackStats is one track's result from Analyze. See ops.TrackStats.
type TrackStats = ops.TrackStats

// AnalyzeReport is the result of Analyze. See ops.AnalyzeReport.
type AnalyzeReport = ops.AnalyzeReport

// Analyze walks path's block headers - never a decoded sample - to compute
// per-track and container-wide stream statistics: exact frame/keyframe
// counts (lacing expanded), byte totals, average/peak bitrate, GOP spans and
// a declared-vs-true duration reconciliation. The walk is head-only: a
// block's payload is seek-skipped, so the cost is proportional to the
// block-header count, never the media volume. See ops.Analyze.
func Analyze(ctx context.Context, path string, opts ...Options) (*AnalyzeReport, error) {
	return ops.Analyze(ctx, path, opts...)
}

// FingerprintReport is the result of Fingerprint. See ops.FingerprintReport.
type FingerprintReport = ops.FingerprintReport

// TrackFingerprint is one track's content identity within a FingerprintReport.
// See ops.TrackFingerprint.
type TrackFingerprint = ops.TrackFingerprint

// Fingerprint computes a container-independent content identity for path: a
// per-track payload SHA-256 (decode order, the same digest CompareBlocks
// proves round-trips with) plus a Presentation hash over all of them, stable
// across container metadata, track-order changes, and container format
// (Matroska/WebM/MP4/MOV) - so a media library can dedup re-muxes of
// identical content regardless of container. Unlike Analyze, this is a full
// read of every track's frame payload. See ops.Fingerprint.
func Fingerprint(ctx context.Context, path string, opts ...Options) (*FingerprintReport, error) {
	return ops.Fingerprint(ctx, path, opts...)
}

// AddAttachment rewrites srcPath to dstPath with att appended to the
// attachments (ID auto-assigned when 0). See ops.AddAttachment.
func AddAttachment(ctx context.Context, srcPath, dstPath string, att Attachment, opts ...Options) error {
	return ops.AddAttachment(ctx, srcPath, dstPath, att, opts...)
}

// RemoveAttachment rewrites srcPath to dstPath without the attachment matching
// target (a decimal ID or an exact name); it errors before writing anything
// when no attachment matches. See ops.RemoveAttachment.
func RemoveAttachment(ctx context.Context, srcPath, dstPath, target string, opts ...Options) error {
	return ops.RemoveAttachment(ctx, srcPath, dstPath, target, opts...)
}

// SetChapters rewrites srcPath to dstPath with its chapters replaced (e.g.
// from ParseOGMChapters). See ops.SetChapters.
func SetChapters(ctx context.Context, srcPath, dstPath string, chapters []Chapter, opts ...Options) error {
	return ops.SetChapters(ctx, srcPath, dstPath, chapters, opts...)
}

// ParseOGMChapters parses the OGM simple chapter text format
// (CHAPTER01=00:00:00.000 / CHAPTER01NAME=Intro). See mkv.ParseOGMChapters.
func ParseOGMChapters(r io.Reader) ([]Chapter, error) { return mkv.ParseOGMChapters(r) }

// FormatOGMChapters renders chapters in the OGM simple chapter text format.
// See mkv.FormatOGMChapters.
func FormatOGMChapters(w io.Writer, chapters []Chapter) error {
	return mkv.FormatOGMChapters(w, chapters)
}

// HashMismatch describes one track whose recomputed content digest does not
// match its stored CONTENT_SHA256 tag. See ops.HashMismatch.
type HashMismatch = ops.HashMismatch

// WriteContentHashes stores each track's content SHA-256 as a CONTENT_SHA256
// tag, making the file self-verifying (dstPath == "" edits in place). See
// ops.WriteContentHashes.
func WriteContentHashes(ctx context.Context, srcPath, dstPath string, opts ...Options) error {
	return ops.WriteContentHashes(ctx, srcPath, dstPath, opts...)
}

// VerifyContentHashes recomputes the per-track content digests and compares
// them with the stored CONTENT_SHA256 tags. See ops.VerifyContentHashes.
func VerifyContentHashes(ctx context.Context, path string, opts ...Options) ([]HashMismatch, error) {
	return ops.VerifyContentHashes(ctx, path, opts...)
}

// CompareBlocks diffs the media CONTENT of two Matroska/WebM files: per-track
// block count, payload byte total and payload SHA-256 in stream order. An
// empty result proves a lossless round-trip. See ops.CompareBlocks.
func CompareBlocks(ctx context.Context, pathA, pathB string, opts ...Options) ([]Diff, error) {
	return ops.CompareBlocks(ctx, pathA, pathB, opts...)
}

// CompareBlocksConcat diffs one file's content against the CONCATENATION of
// several others, in order: the question a split asks - "do these parts, end to
// end, still hold everything the source did?" - answered without building the
// joined file. Tracks are matched by position. See ops.CompareBlocksConcat.
func CompareBlocksConcat(ctx context.Context, path string, parts []string, opts ...Options) ([]Diff, error) {
	return ops.CompareBlocksConcat(ctx, path, parts, opts...)
}

// CompareContainers diffs the metadata of two already-parsed containers  -
// format-agnostic, so an MKV can be compared against an mp4.OpenMeta result to
// verify a remux round-trip. See ops.CompareContainers.
func CompareContainers(a, b *Container) []Diff {
	return ops.CompareContainers(a, b)
}

// Reindex rebuilds the seek index (Cues) of a file. Options.Resync makes it
// tolerate corrupted regions (bounded skip-and-resume, each dropped span
// reported via Options.OnSkip) instead of refusing the file. See ops.Reindex.
func Reindex(ctx context.Context, srcPath, dstPath string, opts ...Options) error {
	return ops.Reindex(ctx, srcPath, dstPath, opts...)
}

// ReindexReplace rebuilds path's seek index through a verified temporary copy,
// then atomically replaces the original (optionally keeping it as path+".bak"
// via Options.KeepBackup). Needs write permission on the directory; see
// ReindexInPlace for the file-only-permission variant. See ops.ReindexReplace.
func ReindexReplace(ctx context.Context, path string, opts ...Options) error {
	return ops.ReindexReplace(ctx, path, opts...)
}

// ErrIndexNotHeadDiscoverable is wrapped into a ReindexInPlace error when the
// file's layout cannot hold a head-discoverable SeekHead, so the patched index
// would not be reachable head-only. The file is rolled back byte-identical.
// errors.Is this to fall back to a full reindex (copy). See ops.ReindexInPlace.
var ErrIndexNotHeadDiscoverable = ops.ErrIndexNotHeadDiscoverable

// ReindexInPlace rebuilds path's seek index by patching the file itself: the
// new Cues element is appended inside the Segment, the head SeekHead repointed
// and any stale Cues voided. Cluster bytes are never moved and no copy of the
// file is created, so it only needs write access to the file (not the
// directory). Crash-safe: every byte about to be overwritten is journaled
// inside the file first, the result is verified (head-only always, full-read
// with Options.DeepVerify) while the journal still allows a rollback, and the
// journal is truncated away only after the checks pass. Any failure restores
// the original bytes. See ops.ReindexInPlace.
func ReindexInPlace(ctx context.Context, path string, opts ...Options) error {
	return ops.ReindexInPlace(ctx, path, opts...)
}

// RecoverInPlace rolls back an interrupted ReindexInPlace: if path still
// carries the in-file journal (the process died mid-operation), the original
// bytes are restored and the journal removed. Returns false when the file
// carries no journal (nothing to do). See ops.RecoverInPlace.
func RecoverInPlace(ctx context.Context, path string, opts ...Options) (bool, error) {
	return ops.RecoverInPlace(ctx, path, opts...)
}

// DamagedRange is one span of srcPath a Salvage run could not carry over
// verbatim. See ops.DamagedRange.
type DamagedRange = ops.DamagedRange

// RepairedRange is one region a tolerant walk reconstructed instead of
// copying verbatim (corrected sizes, continuation headers around a gap),
// with the media inside kept verbatim. See ops.RepairedRange.
type RepairedRange = ops.RepairedRange

// SalvageReport summarises one Salvage run. See ops.SalvageReport.
type SalvageReport = ops.SalvageReport

// Salvage produces a best-effort copy of a damaged Matroska/WebM file:
// metadata and cluster payloads are carried over verbatim and the Cues index
// is rebuilt, exactly like Reindex, but a structural failure inside the
// cluster stream is NOT fatal - Salvage scans forward for the next valid
// Cluster and resumes, recording the skipped span in the returned report
// instead of aborting. Prefer Reindex (which refuses mid-file corruption by
// design) whenever the source is expected to be intact; reach for Salvage
// only once Reindex/Validate have confirmed damage. See ops.Salvage.
func Salvage(ctx context.Context, srcPath, dstPath string, opts ...Options) (*SalvageReport, error) {
	return ops.Salvage(ctx, srcPath, dstPath, opts...)
}

// MapDamage runs the exact walk Salvage runs but writes nothing: a dry-run
// that reports what a repair would keep, reconstruct, and lose, so the
// decision to repair can be made with the numbers in hand. See ops.MapDamage.
func MapDamage(ctx context.Context, srcPath string, opts ...Options) (*SalvageReport, error) {
	return ops.MapDamage(ctx, srcPath, opts...)
}

// RollbackInfo summarises the delta entry a repair wrote to
// Options.RollbackSink. See mkv.RollbackInfo.
type RollbackInfo = mkv.RollbackInfo

// CueHealthReport classifies a file's CuePoints by referenced track and reports
// the video cues' coverage: the worst hole and where it opens (MaxVideoGapMs/
// MaxVideoGapAtMs), the tail measured to the picture's own end rather than the
// declared duration (VideoEndMs/VideoEndExact/TailGapMs), and picture the
// track's statistics say is missing from the stream (VideoShortfallMs). See
// ops.CueHealthReport.
type CueHealthReport = ops.CueHealthReport

// CueHealth judges the seek index head-only (no cluster walk): can it seek
// video? It spots an index that exists yet keys on no video track at all -
// present, non-empty, useless for seeking - and one whose video cues are too
// far apart to land near a target. Cues on other tracks are counted, never held
// against a file: seeking never uses them. The scan-time complement of Validate.
// See ops.CueHealth.
func CueHealth(ctx context.Context, path string, opts ...Options) (*CueHealthReport, error) {
	return ops.CueHealth(ctx, path, opts...)
}

// Finding is one diagnosed defect with its remedy. See ops.Finding.
type Finding = ops.Finding

// Diagnosis is Diagnose's full triage verdict for one file. See ops.Diagnosis.
type Diagnosis = ops.Diagnosis

// Diagnose classifies a file in one call - seek-index health, per-track
// audio start delays, declared-size coherence, and (only when the size check
// suggests damage) the full tolerant walk - and names the remedy for every
// finding (reindex / retime / resync / re-download). Head-mostly. A .mkv
// whose content is really ISO base media (MP4/MOV) is classified rather than
// failed: one "wrong-container" finding (remedy: rename/remux), so a scan
// settles the file instead of erroring on it every pass. See ops.Diagnose.
func Diagnose(ctx context.Context, path string, opts ...Options) (*Diagnosis, error) {
	return ops.Diagnose(ctx, path, opts...)
}

// AudioStartDelays returns, per audio track number, how late the track's
// content starts relative to the first video keyframe, in nanoseconds
// (positive = audio late; hand the negated values to RetimeTracks to cancel
// the delay). Track numbers and delays come from the same parse.
// See ops.AudioStartDelays.
func AudioStartDelays(ctx context.Context, path string, opts ...Options) (map[uint64]int64, error) {
	return ops.AudioStartDelays(ctx, path, opts...)
}

// RetimeTracks shifts the block timecodes of the given tracks (track number
// -> shift in nanoseconds, negative = earlier) - the fix for a constant A/V
// desync (audio content starting late). It picks the cheaper engine
// automatically: the in-place 2-bytes-per-block patch when patches are few,
// the sequential rewrite when they are dense (multi-track movies).
// See ops.RetimeTracks.
func RetimeTracks(ctx context.Context, path string, shift map[uint64]int64, opts ...Options) error {
	return ops.RetimeTracks(ctx, path, shift, opts...)
}

// The retime refusal classes, re-exported so a caller routes on errors.Is
// instead of parsing message text. Each is PERMANENT for the same (file,
// shift) call - retrying it cannot succeed - unlike I/O failures or a
// leftover temporary file, which are never wrapped in these. Two more typed
// causes complete the set: ErrCorruptSource (the file needs a repair first)
// and ErrNotMatroska (the file is not Matroska at all). See ops for the
// per-sentinel contracts.
var (
	// ErrUnknownSizeSegment: streamed/unsealed Segment, in-place engines
	// only; RetimeTracks routes to the rewrite on it automatically.
	ErrUnknownSizeSegment = ops.ErrUnknownSizeSegment
	// ErrUnknownTrack: the shift map names a track the file does not have.
	ErrUnknownTrack = ops.ErrUnknownTrack
	// ErrTrackHasNoBlocks: the named track exists but has nothing to shift.
	ErrTrackHasNoBlocks = ops.ErrTrackHasNoBlocks
	// ErrShiftNotRepresentable: the shift is not a whole number of timecode
	// ticks at the file's TimecodeScale (or rounds to zero).
	ErrShiftNotRepresentable = ops.ErrShiftNotRepresentable
	// ErrShiftOutOfRange: the shift would push a block past the int16
	// cluster-relative window or to a negative absolute timestamp.
	ErrShiftOutOfRange = ops.ErrShiftOutOfRange
	// ErrCorruptSource: the source itself does not parse; repair it first
	// (Reindex with Options.Resync, or Salvage), then retry.
	ErrCorruptSource = ops.ErrCorruptSource
)

// RetimeTracksInPlace forces the in-place engine: 2 bytes patched per block
// under the crash-safe journal, no rewrite, file-only permission.
// See ops.RetimeTracksInPlace.
func RetimeTracksInPlace(ctx context.Context, path string, shift map[uint64]int64, opts ...Options) error {
	return ops.RetimeTracksInPlace(ctx, path, shift, opts...)
}

// RetimeTracksReplace forces the sequential rewrite: timecodes patched on
// the fly, the seek index rebuilt healthy, verified then atomically swapped
// (KeepBackup keeps the original). Trailing junk past the declared Segment
// end is dropped and reported through Options.OnSkip instead of blocking the
// repair. See ops.RetimeTracksReplace.
func RetimeTracksReplace(ctx context.Context, path string, shift map[uint64]int64, opts ...Options) error {
	return ops.RetimeTracksReplace(ctx, path, shift, opts...)
}

// ApplyRollback reconstructs the pre-repair original from a repaired file
// and the delta entry the repair wrote to Options.RollbackSink. It refuses
// to reconstruct when the repaired file changed since the repair, and never
// delivers a reconstruction that does not hash back to the original.
// See ops.ApplyRollback.
func ApplyRollback(ctx context.Context, repairedPath string, delta io.Reader, dstPath string, opts ...Options) error {
	return ops.ApplyRollback(ctx, repairedPath, delta, dstPath, opts...)
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

// ExtractSubtitleWebVTT writes an embedded Matroska/WebM text subtitle track as
// WebVTT to w. See ops.ExtractSubtitleWebVTT.
func ExtractSubtitleWebVTT(ctx context.Context, srcPath string, trackID uint64, w io.Writer, opts ...Options) error {
	return ops.ExtractSubtitleWebVTT(ctx, srcPath, trackID, w, opts...)
}

// SubtitleFileToWebVTT converts an external subtitle sidecar (.srt/.ass/.ssa/.vtt)
// to WebVTT, written to w. See subtitle.FileToWebVTT.
func SubtitleFileToWebVTT(srcPath string, w io.Writer) error {
	return subtitle.FileToWebVTT(srcPath, w)
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
