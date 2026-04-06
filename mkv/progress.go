package mkv

// ProgressFunc is called during long operations with bytes processed and total.
// Total is -1 if unknown.
type ProgressFunc func(processed, total int64)

// Options holds optional parameters for long-running operations.
type Options struct {
	Progress ProgressFunc
	FS       *FS
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
