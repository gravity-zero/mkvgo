package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
)

const reindexInPlaceUsage = "usage: mkvgo reindex-inplace <file.mkv> [--deep-verify] [--rollback]"

// CmdReindexInPlace rebuilds the Cues index by patching the file directly
// (crash-safe in-file journal, automatic rollback on any failed check).
// --rollback only restores a file left mid-operation by a crash.
func CmdReindexInPlace(args []string) {
	var deepVerify, rollback bool
	var rest []string
	for _, a := range args {
		switch a {
		case "--deep-verify":
			deepVerify = true
		case "--rollback":
			rollback = true
		default:
			rejectFlagArg(a)
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		Fatal(reindexInPlaceUsage)
	}
	path := rest[0]

	if rollback {
		if deepVerify {
			Fatal("--rollback only restores an interrupted run; it does not reindex, so --deep-verify does not apply")
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

	opts := matroska.Options{Progress: NewProgressBar(), DeepVerify: deepVerify}
	err := matroska.ReindexInPlace(context.Background(), path, opts)
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("reindexed %s in place (index patched, clusters untouched)\n", path)
}
