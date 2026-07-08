package mkv

// ProgressFunc is called during long operations with bytes processed and total.
// Total is -1 if unknown.
type ProgressFunc func(processed, total int64)

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
