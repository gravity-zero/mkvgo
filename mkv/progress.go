package mkv

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
	StartOffset   int64
	EndOffset     int64
	ApproxStartMs int64
	ApproxEndMs   int64
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
