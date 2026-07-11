package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
)

const reindexUsage = "usage: mkvgo reindex <input.mkv> [output.mkv] [--deep-verify] [--replace] [--keep-backup] [--resync] [--clean-cut] [--strict] [--rollback-delta <file>]"

func CmdReindex(args []string) {
	var deepVerify, replace, keepBackup, resync, cleanCut, strict bool
	var deltaPath string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--deep-verify":
			deepVerify = true
		case "--strict":
			strict = true
		case "--replace":
			replace = true
		case "--keep-backup":
			keepBackup = true
		case "--resync":
			resync = true
		case "--clean-cut":
			cleanCut = true
		case "--rollback-delta":
			if i+1 >= len(args) {
				Fatal("--rollback-delta needs a file path")
			}
			i++
			deltaPath = args[i]
		default:
			rejectFlagArg(args[i])
			rest = append(rest, args[i])
		}
	}
	if len(rest) < 1 {
		Fatal(reindexUsage)
	}
	if cleanCut && !resync {
		Fatal("--clean-cut only applies together with --resync")
	}

	var skipped []matroska.DamagedRange
	var repaired []matroska.RepairedRange
	opts := matroska.Options{Progress: NewProgressBar(), DeepVerify: deepVerify, KeepBackup: keepBackup, Resync: resync, CleanCut: cleanCut, StrictVerify: strict}
	printPreexisting := armPreexisting(&opts)
	if resync {
		opts.OnSkip = func(r matroska.DamagedRange) { skipped = append(skipped, r) }
		opts.OnRepair = func(r matroska.RepairedRange) { repaired = append(repaired, r) }
	}
	printDelta := func() {}
	if deltaPath != "" {
		var closeDelta func()
		printDelta, closeDelta = armRollbackDelta(&opts, deltaPath)
		defer closeDelta()
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
		printPreexisting()
		printResyncOutcome(resync, skipped, repaired)
		printDelta()
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
	printPreexisting()
	printResyncOutcome(resync, skipped, repaired)
	printDelta()
}

// printResyncOutcome reports what a --resync run repaired and dropped, so the
// operator knows exactly which byte ranges (and roughly which presentation
// times) were reconstructed or lost. No-op without --resync.
func printResyncOutcome(resync bool, skipped []matroska.DamagedRange, repaired []matroska.RepairedRange) {
	if !resync {
		return
	}
	for i, r := range repaired {
		fmt.Printf("  repaired range %d: offset %d-%d, %s of media kept that a plain resync would have dropped\n",
			i+1, r.StartOffset, r.EndOffset, FormatBytes(r.BytesKept))
	}
	if len(skipped) == 0 {
		if len(repaired) > 0 {
			fmt.Println("  nothing lost (repairs were lossless)")
		} else {
			fmt.Println("  no corrupted region found (resync was not needed)")
		}
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
