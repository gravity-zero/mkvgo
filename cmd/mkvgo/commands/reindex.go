package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
)

const reindexUsage = "usage: mkvgo reindex <input.mkv> [output.mkv] [--deep-verify] [--replace] [--keep-backup] [--resync]"

func CmdReindex(args []string) {
	var deepVerify, replace, keepBackup, resync bool
	var rest []string
	for _, a := range args {
		switch a {
		case "--deep-verify":
			deepVerify = true
		case "--replace":
			replace = true
		case "--keep-backup":
			keepBackup = true
		case "--resync":
			resync = true
		default:
			rejectFlagArg(a)
			rest = append(rest, a)
		}
	}
	if len(rest) < 1 {
		Fatal(reindexUsage)
	}

	var skipped []matroska.DamagedRange
	opts := matroska.Options{Progress: NewProgressBar(), DeepVerify: deepVerify, KeepBackup: keepBackup, Resync: resync}
	if resync {
		opts.OnSkip = func(r matroska.DamagedRange) { skipped = append(skipped, r) }
	}

	if replace {
		if len(rest) != 1 {
			Fatal("--replace takes exactly one path: the source is reindexed in place, no separate output")
		}
		src := rest[0]
		err := matroska.ReindexReplace(context.Background(), src, opts)
		ClearProgress()
		if err != nil {
			Fatal(err.Error())
		}
		fmt.Printf("reindexed %s (original replaced after verification)\n", src)
		printSkippedRanges(resync, skipped)
		return
	}

	if keepBackup {
		Fatal("--keep-backup only applies together with --replace")
	}
	if len(rest) < 2 {
		Fatal(reindexUsage)
	}
	src, dst := rest[0], rest[1]
	GuardOverwrite(dst)

	err := matroska.Reindex(context.Background(), src, dst, opts)
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("reindexed %s → %s\n", src, dst)
	printSkippedRanges(resync, skipped)
}

// printSkippedRanges reports what a --resync run dropped, so the operator
// knows exactly which byte ranges (and roughly which presentation times) the
// repaired file no longer carries. No-op without --resync.
func printSkippedRanges(resync bool, skipped []matroska.DamagedRange) {
	if !resync {
		return
	}
	if len(skipped) == 0 {
		fmt.Println("  no corrupted region found (resync was not needed)")
		return
	}
	var total int64
	for i, r := range skipped {
		total += r.EndOffset - r.StartOffset
		fmt.Printf("  skipped range %d: offset %d-%d (%s), approx %s-%s\n",
			i+1, r.StartOffset, r.EndOffset, FormatBytes(r.EndOffset-r.StartOffset),
			FmtMs(r.ApproxStartMs), FmtMs(r.ApproxEndMs))
	}
	fmt.Printf("  %s of corrupted data skipped across %d range(s)\n", FormatBytes(total), len(skipped))
}
