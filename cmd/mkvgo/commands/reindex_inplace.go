package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
)

const reindexInPlaceUsage = "usage: mkvgo reindex-inplace <file.mkv> [--deep-verify] [--rollback] [--strict] [--rollback-delta <file>]"

// CmdReindexInPlace rebuilds the Cues index by patching the file directly
// (crash-safe in-file journal, automatic rollback on any failed check).
// --rollback only restores a file left mid-operation by a crash.
// --rollback-delta persists the journal as an inverse delta so the repair
// stays reversible after it commits (see the rollback command).
func CmdReindexInPlace(args []string) {
	var deepVerify, rollback, strict bool
	var deltaPath string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--deep-verify":
			deepVerify = true
		case "--strict":
			strict = true
		case "--rollback":
			rollback = true
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
	if len(rest) != 1 {
		Fatal(reindexInPlaceUsage)
	}
	path := rest[0]

	if rollback {
		if deepVerify || deltaPath != "" {
			Fatal("--rollback only restores an interrupted run; it does not reindex, so --deep-verify and --rollback-delta do not apply")
		}
		recovered, err := matroska.RecoverInPlace(context.Background(), path)
		if err != nil {
			Fatal(err.Error())
		}
		if recovered {
			fmt.Printf("rolled back interrupted in-place reindex of %s (original bytes restored)\n", path)
		} else {
			fmt.Printf("%s carries no journal, nothing to roll back\n", path)
		}
		return
	}

	opts := matroska.Options{Progress: NewProgressBar(), DeepVerify: deepVerify, StrictVerify: strict}
	printPreexisting := armPreexisting(&opts)
	printDelta := func() {}
	if deltaPath != "" {
		var closeDelta func()
		printDelta, closeDelta = armRollbackDelta(&opts, deltaPath)
		defer closeDelta()
	}
	err := matroska.ReindexInPlace(context.Background(), path, opts)
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("reindexed %s in place (index patched, clusters untouched)\n", path)
	printPreexisting()
	printDelta()
}
