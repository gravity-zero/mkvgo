package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
)

func CmdReindex(args []string) {
	if len(args) < 2 {
		Fatal("usage: mkvgo reindex <input.mkv> <output.mkv>")
	}
	src, dst := args[0], args[1]
	GuardOverwrite(dst)

	err := matroska.Reindex(context.Background(), src, dst, matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("reindexed %s → %s\n", src, dst)
}
