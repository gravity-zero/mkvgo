package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
)

const reindexUsage = "usage: mkvgo reindex <input.mkv> [output.mkv] [--deep-verify] [--replace] [--keep-backup]"

func CmdReindex(args []string) {
	var deepVerify, replace, keepBackup bool
	var rest []string
	for _, a := range args {
		switch a {
		case "--deep-verify":
			deepVerify = true
		case "--replace":
			replace = true
		case "--keep-backup":
			keepBackup = true
		default:
			rejectFlagArg(a)
			rest = append(rest, a)
		}
	}
	if len(rest) < 1 {
		Fatal(reindexUsage)
	}

	opts := matroska.Options{Progress: NewProgressBar(), DeepVerify: deepVerify, KeepBackup: keepBackup}

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
}
