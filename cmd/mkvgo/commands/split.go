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
	var outDir, pattern string
	var byChapters bool
	var everyMs int64
	var ranges []matroska.TimeRange

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			if i >= len(args) {
				Fatal("-o needs a value")
			}
			outDir = args[i]
		case "-chapters":
			byChapters = true
		case "-range":
			i++
			if i >= len(args) {
				Fatal("-range needs a value")
			}
			ranges = ParseTimeRanges(args[i])
		case "-every":
			i++
			if i >= len(args) {
				Fatal("-every needs a value")
			}
			ms, err := ParseTimePoint(args[i])
			if err != nil || ms <= 0 {
				Fatal(fmt.Sprintf("invalid -every duration %q", args[i]))
			}
			everyMs = ms
		case "-pattern":
			i++
			if i >= len(args) {
				Fatal("-pattern needs a value")
			}
			pattern = args[i]
		default:
			rejectFlagArg(args[i])
		}
	}
	if outDir == "" {
		Fatal("missing -o <dir>")
	}
	modes := 0
	for _, on := range []bool{byChapters, len(ranges) > 0, everyMs > 0} {
		if on {
			modes++
		}
	}
	if modes != 1 {
		Fatal("specify exactly one of -chapters, -range or -every")
	}

	outputs, err := matroska.Split(context.Background(), matroska.SplitOptions{
		SourcePath: source,
		OutputDir:  outDir,
		ByChapters: byChapters,
		Ranges:     ranges,
		EveryMs:    everyMs,
		Pattern:    pattern,
	}, matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
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
			if i >= len(args) {
				Fatal("-pattern needs a value")
			}
			outPath = args[i]
		} else {
			rejectFlagArg(args[i])
			sources = append(sources, args[i])
		}
	}
	if outPath == "" || len(sources) == 0 {
		Fatal("usage: mkvgo join -o <out.mkv> <file1.mkv> <file2.mkv> ...")
	}
	GuardOverwrite(outPath)

	err := matroska.Join(context.Background(), sources, outPath, matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("joined %d files → %s\n", len(sources), outPath)
}
