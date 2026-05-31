package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/ops"
)

func CmdReindex(args []string) {
	if len(args) < 2 {
		Fatal("usage: mkvgo reindex <input.mkv> <output.mkv>")
	}
	src, dst := args[0], args[1]

	err := ops.Reindex(context.Background(), src, dst, mkv.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("reindexed %s → %s\n", src, dst)
}
