package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
)

func CmdSplit(args []string) {
	if len(args) < 3 {
		Fatal("usage: mkvgo split <file.mkv> -o <dir> [-chapters | -range 0-5000,5000-0]")
	}
	source := args[0]
	var outDir string
	var byChapters bool
	var ranges []matroska.TimeRange

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			outDir = args[i]
		case "-chapters":
			byChapters = true
		case "-range":
			i++
			ranges = ParseTimeRanges(args[i])
		}
	}
	if outDir == "" {
		Fatal("missing -o <dir>")
	}
	if !byChapters && len(ranges) == 0 {
		Fatal("specify -chapters or -range")
	}

	outputs, err := matroska.Split(context.Background(), matroska.SplitOptions{
		SourcePath: source,
		OutputDir:  outDir,
		ByChapters: byChapters,
		Ranges:     ranges,
	})
	if err != nil {
		Fatal(err.Error())
	}
	for i, p := range outputs {
		fmt.Printf("part %d → %s\n", i+1, p)
	}
}

func CmdJoin(args []string) {
	if len(args) < 3 {
		Fatal("usage: mkvgo join -o <out.mkv> <file1.mkv> <file2.mkv> ...")
	}
	var outPath string
	var sources []string

	for i := 0; i < len(args); i++ {
		if args[i] == "-o" {
			i++
			outPath = args[i]
		} else {
			sources = append(sources, args[i])
		}
	}
	if outPath == "" || len(sources) == 0 {
		Fatal("usage: mkvgo join -o <out.mkv> <file1.mkv> <file2.mkv> ...")
	}

	err := matroska.Join(context.Background(), sources, outPath)
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("joined %d files → %s\n", len(sources), outPath)
}
