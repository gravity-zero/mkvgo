package mkv

// reports.go - the report types the repair and triage operations return.
// They live here (rather than in mkv/ops, which computes most of them) so
// that BOTH engines can speak the same shapes: ops imports mp4 for the
// packaging-aware operations, so the mp4 package cannot import ops - but a
// MP4 triage must still return the exact Diagnosis a Matroska triage
// returns, or a caller scanning a mixed library needs two code paths.
// mkv/ops re-exports each of these as a type alias, so existing consumers
// see no change.

// SalvageReport summarises one tolerant walk (ops.Salvage, ops.MapDamage,
// or Reindex with Options.Resync).
type SalvageReport struct {
	ClustersCopied int            `json:"clusters_copied"`
	BytesCopied    int64          `json:"bytes_copied"`
	BytesSkipped   int64          `json:"bytes_skipped"`
	DamagedRanges  []DamagedRange `json:"damaged_ranges"`
	// RepairedRanges lists the regions where cluster framing was re-derived
	// from the bytes (corrected sizes, continuation headers around a gap)
	// instead of dropping the whole declared extent; the media inside is
	// verbatim. Empty on a clean source.
	RepairedRanges []RepairedRange `json:"repaired_ranges"`
	// CleanCutBytes counts video bytes intentionally dropped after damage
	// gaps because they precede the next video keyframe (Options.CleanCut).
	CleanCutBytes int64 `json:"clean_cut_bytes"`
	// TruncatedTail is the first-class "incomplete download" verdict: the
	// final damaged range runs to the end of the file AND begins inside the
	// declared Segment - the missing tail is not recoverable by ANY tool,
	// only by re-acquiring the source. Mid-file damage without this flag is
	// repairable in full (resync). Unparseable bytes lying entirely PAST the
	// declared Segment end never set it: those are surplus bytes (trailing
	// junk, a crashed in-place journal), still counted in DamagedRanges and
	// BytesSkipped, but dropping them loses nothing.
	TruncatedTail bool `json:"truncated_tail"`
}

// CueHealthReport classifies a Matroska file's CuePoints by referenced track
// (ops.CueHealth).
type CueHealthReport struct {
	TotalCues    int `json:"total_cues"`
	VideoCues    int `json:"video_cues"`
	NonVideoCues int `json:"non_video_cues"`
	// UnknownTrackCues counts cues referencing a track number absent from
	// the Tracks element (a stale or foreign index).
	UnknownTrackCues int `json:"unknown_track_cues"`
	// NonVideoPct is NonVideoCues+UnknownTrackCues over TotalCues, in percent.
	// Reporting only: cues keyed on another track are inert for seeking (the
	// keyframe index consumes the video-keyed ones alone), so a high share is
	// index bloat, not a seek defect. The verdict judges the video cues.
	NonVideoPct float64 `json:"non_video_pct"`
	// PerTrack counts cues per referenced track number.
	PerTrack map[uint64]int `json:"per_track"`
	// FirstCueMs/LastCueMs bracket the index's time coverage.
	FirstCueMs int64 `json:"first_cue_ms"`
	LastCueMs  int64 `json:"last_cue_ms"`
	// MaxVideoGapMs is the widest hole in the VIDEO cue coverage - the worst
	// distance a seek can land from its target - measured between consecutive
	// video cues, and from 0 to the first and from the last to the file's
	// duration when it is known. 0 when the file has no video track.
	MaxVideoGapMs int64 `json:"max_video_gap_ms"`
	// HasVideoTrack tells which rule applied: a video file's index must be able
	// to seek video; an audio-only file legitimately cues audio.
	HasVideoTrack bool `json:"has_video_track"`
	// Healthy is the verdict; Reason says why not, with the remedy.
	Healthy bool   `json:"healthy"`
	Reason  string `json:"reason,omitempty"`
}

// Finding is one diagnosed defect with its remedy.
type Finding struct {
	// Kind: "no-index" | "index-misskeyed" | "index-sparse" |
	// "index-stale-tracks" | "audio-delay" | "truncated" | "damaged" |
	// "trailing-junk" | "streamed-size" | "no-moov" | "wrong-container".
	Kind    string `json:"kind"`
	Detail  string `json:"detail"`
	Remedy  string `json:"remedy"`
	Track   uint64 `json:"track,omitempty"`
	DelayNs int64  `json:"delay_ns,omitempty"`
}

// Diagnosis is the full triage verdict for one file - the same shape for
// Matroska (ops.Diagnose) and MP4 (mp4.Diagnose) sources, so one scan loop
// covers a mixed library.
type Diagnosis struct {
	Healthy  bool      `json:"healthy"`
	Findings []Finding `json:"findings"`
	// CueHealth is the index classification behind the index findings.
	// Matroska only: an MP4's sample table is its index by construction.
	CueHealth *CueHealthReport `json:"cue_health,omitempty"`
	// AudioDelaysNs holds every audio track's start delay relative to the
	// video, whether or not it crossed the finding threshold.
	AudioDelaysNs map[uint64]int64 `json:"audio_delays_ns"`
	// Damage is the tolerant-walk report, present only when the size check
	// warranted the walk (Matroska only).
	Damage *SalvageReport `json:"damage,omitempty"`
}
