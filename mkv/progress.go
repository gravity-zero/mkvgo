package mkv

import "io"

// ProgressFunc is called during long operations with bytes processed and total.
// Total is -1 if unknown.
type ProgressFunc func(processed, total int64)

// DamagedRange records one span of a source file that a tolerant walk
// (ops.Salvage, or Reindex with Options.Resync) could not carry over verbatim
// - either because the byte range itself failed a structural check, or
// because a resync scan had to jump over it to reach the next good Cluster.
// Offsets are absolute byte offsets into the source path. ApproxStartMs and
// ApproxEndMs bracket the presentation time lost: the last known-good
// cluster's timestamp before the gap and the first known-good cluster's
// timestamp after it (or, for a truncated tail, the last known-good
// timestamp repeated, since there is no "after").
type DamagedRange struct {
	StartOffset   int64 `json:"start_offset"`
	EndOffset     int64 `json:"end_offset"`
	ApproxStartMs int64 `json:"approx_start_ms"`
	ApproxEndMs   int64 `json:"approx_end_ms"`
}

// RepairedRange records one region that a tolerant walk (ops.Salvage, or
// Reindex with Options.Resync) reconstructed instead of copying verbatim: the
// cluster framing there was re-derived from the bytes (corrected sizes,
// synthesized continuation headers around a gap), while the media bytes
// inside the kept runs remain verbatim. BytesKept counts the media bytes
// preserved that a plain skip-to-next-cluster resync would have dropped.
type RepairedRange struct {
	StartOffset int64 `json:"start_offset"`
	EndOffset   int64 `json:"end_offset"`
	BytesKept   int64 `json:"bytes_kept"`
}

// RollbackInfo summarises the delta entry a repair wrote to
// Options.RollbackSink: its size and the sha256 of the original (pre-repair)
// and repaired files that gate its application.
type RollbackInfo struct {
	Bytes     int64    `json:"bytes"`
	SrcSHA256 [32]byte `json:"src_sha256"`
	DstSHA256 [32]byte `json:"dst_sha256"`
}

// Options holds optional parameters for long-running operations.
type Options struct {
	Progress ProgressFunc
	FS       *FS
	// DeepVerify makes the reindex operations run a full-read validation of
	// the result (every cue checked against real video keyframes) on top of
	// the always-on head-only check; Reindex additionally proves the cluster
	// payloads byte-identical to the source. Costs a full read of the output
	// (and of both files for Reindex). A failure aborts before any replace
	// (Reindex/ReindexReplace) or rolls the patch back (ReindexInPlace).
	DeepVerify bool
	// KeepBackup makes ReindexReplace keep the original file as path+".bak"
	// instead of overwriting it in place.
	KeepBackup bool
	// Resync makes Reindex (and ReindexReplace, which delegates to it)
	// tolerate corrupted regions in the source's cluster stream: instead of
	// refusing the file on the first element that does not decode, the walk
	// scans forward (bounded) for the next structurally valid Cluster and
	// resumes there, dropping the skipped bytes from the output. Off by
	// default - the strict refusal stays the contract. The repair is still
	// refused when no valid Cluster is found within the scan window, when no
	// cluster survives at all, or when more than half of the walked payload
	// would be dropped: a mostly-damaged file must not silently "repair" into
	// a stub (use ops.Salvage for best-effort recovery without that cap).
	// Not supported by ReindexInPlace, which cannot drop bytes from the file
	// itself.
	Resync bool
	// OnSkip, when non-nil, is called once per source range that a Resync
	// reindex dropped, after the copy and all its verifications have passed.
	// It is never called when Resync is unset or when the source was clean.
	OnSkip func(DamagedRange)
	// OnRepair, when non-nil, is called once per region a Resync reindex
	// reconstructed with zero or partial loss (see RepairedRange), after all
	// verifications have passed. Never called when Resync is unset.
	OnRepair func(RepairedRange)
	// CleanCut makes the tolerant walks (Salvage, Reindex with Resync)
	// resume video only at the next video keyframe after a damage gap: the
	// first recovered video blocks after a gap are often P/B frames that
	// reference lost frames and decode with artifacts until the next
	// keyframe. Audio blocks resume immediately (each frame is independent).
	// The dropped video bytes are counted in the report (CleanCutBytes) and
	// the damaged range's approximate end time extends to the keyframe.
	CleanCut bool
	// RollbackSink, when non-nil, receives one framed delta entry during the
	// repair: the recipe to reconstruct the pre-repair ORIGINAL from the
	// repaired output (see ops.ApplyRollback), typically under a MB where a
	// full backup copy would be the whole file. Supported by Reindex and
	// ReindexReplace (strict and Resync paths), Salvage, and ReindexInPlace;
	// ignored by MapDamage (nothing is written to roll back). Emitting the
	// entry costs one extra sequential read of the repaired file (its
	// sha256, which gates the rollback). Nil = no delta.
	RollbackSink io.Writer
	// RollbackRequired makes a RollbackSink failure (write error, delta
	// bigger than its buffer cap) fail the whole repair. Default false: the
	// delta is best-effort and the repair proceeds without it (OnRollback is
	// then never called; the caller keeps its full-copy fallback).
	RollbackRequired bool
	// OnRollback, when non-nil, is called once with the written delta
	// entry's summary after the repair and all its verifications passed.
	OnRollback func(RollbackInfo)
}

func ProgressFrom(opts []Options) ProgressFunc {
	if len(opts) > 0 && opts[0].Progress != nil {
		return opts[0].Progress
	}
	return nil
}

func FSFrom(opts []Options) *FS {
	if len(opts) > 0 && opts[0].FS != nil {
		return opts[0].FS
	}
	return nil
}

func DeepVerifyFrom(opts []Options) bool {
	return len(opts) > 0 && opts[0].DeepVerify
}

func KeepBackupFrom(opts []Options) bool {
	return len(opts) > 0 && opts[0].KeepBackup
}

func ResyncFrom(opts []Options) bool {
	return len(opts) > 0 && opts[0].Resync
}

func OnSkipFrom(opts []Options) func(DamagedRange) {
	if len(opts) > 0 && opts[0].OnSkip != nil {
		return opts[0].OnSkip
	}
	return nil
}

func OnRepairFrom(opts []Options) func(RepairedRange) {
	if len(opts) > 0 && opts[0].OnRepair != nil {
		return opts[0].OnRepair
	}
	return nil
}

func CleanCutFrom(opts []Options) bool {
	return len(opts) > 0 && opts[0].CleanCut
}

func RollbackSinkFrom(opts []Options) io.Writer {
	if len(opts) > 0 {
		return opts[0].RollbackSink
	}
	return nil
}

func RollbackRequiredFrom(opts []Options) bool {
	return len(opts) > 0 && opts[0].RollbackRequired
}

func OnRollbackFrom(opts []Options) func(RollbackInfo) {
	if len(opts) > 0 && opts[0].OnRollback != nil {
		return opts[0].OnRollback
	}
	return nil
}
