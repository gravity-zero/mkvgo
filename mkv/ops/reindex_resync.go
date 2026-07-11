package ops

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/mkv"
)

// reindexResyncMaxSkipPercent caps how much of the walked payload a Resync
// reindex may drop before refusing the repair: a file that is mostly garbage
// must surface as damaged, not silently "repair" into a stub. Salvage is the
// escape hatch for callers who want best-effort recovery past this point.
const reindexResyncMaxSkipPercent = 50

// reindexResync is the Options.Resync path of Reindex: a tolerant copy that
// surgically repairs or skips corrupted regions (the same walk Salvage uses),
// then enforces the reindex guarantees on top - a surviving-payload cap, the
// always-on cue verification and the optional deep pass. Skipped and repaired
// source ranges are reported through Options.OnSkip and Options.OnRepair only
// after every check passed.
func reindexResync(ctx context.Context, srcPath, dstPath string, fs *mkv.FS, opts []mkv.Options) error {
	report, cues, timecodeScale, err := salvageCopy(ctx, srcPath, dstPath, fs, mkv.ProgressFrom(opts), mkv.CleanCutFrom(opts))
	if err != nil {
		return fmt.Errorf("reindex resync: %w", err)
	}

	if report.ClustersCopied == 0 {
		return fmt.Errorf("reindex resync: no cluster survived the walk (%d bytes skipped); the file is damaged beyond a reindex repair", report.BytesSkipped)
	}
	walked := report.BytesCopied + report.BytesSkipped
	if report.BytesSkipped*100 > walked*reindexResyncMaxSkipPercent {
		return fmt.Errorf("reindex resync: %d of %d walked bytes would be dropped (more than %d%%); refusing to repair a mostly-damaged file (use Salvage for best-effort recovery)",
			report.BytesSkipped, walked, reindexResyncMaxSkipPercent)
	}

	if err := verifyReindexedCues(ctx, dstPath, fs, cues, timecodeScale); err != nil {
		return err
	}

	if mkv.DeepVerifyFrom(opts) {
		if err := deepVerifyValidate(ctx, dstPath, fs); err != nil {
			return err
		}
		// The verbatim proof compares every block against the source, which
		// cannot be walked past its damage (and the output legitimately lacks
		// the dropped blocks) - it is only meaningful when nothing was
		// skipped, repaired, or clean-cut.
		if len(report.DamagedRanges) == 0 && len(report.RepairedRanges) == 0 && report.CleanCutBytes == 0 {
			if err := deepVerifyVerbatim(ctx, srcPath, dstPath, fs); err != nil {
				return err
			}
		}
	}

	if onSkip := mkv.OnSkipFrom(opts); onSkip != nil {
		for _, r := range report.DamagedRanges {
			onSkip(r)
		}
	}
	if onRepair := mkv.OnRepairFrom(opts); onRepair != nil {
		for _, r := range report.RepairedRanges {
			onRepair(r)
		}
	}
	return nil
}
