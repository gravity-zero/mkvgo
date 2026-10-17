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

	fl := newFlags("split")
	fl.String(&outDir, "-o")
	fl.Bool(&byChapters, "-chapters")
	fl.Func(func(v string) { ranges = ParseTimeRanges(v) }, "-range")
	fl.Func(func(v string) {
		ms, err := ParseTimePoint(v)
		if err != nil || ms <= 0 {
			Fatal(fmt.Sprintf("invalid -every duration %q", v))
		}
		everyMs = ms
	}, "-every")
	fl.String(&pattern, "-pattern")
	fl.Parse(args[1:])
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
		FatalErr(err)
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

	fl := newFlags("join")
	fl.String(&outPath, "-o")
	sources = fl.Parse(args)
	if outPath == "" || len(sources) == 0 {
		Fatal("usage: mkvgo join -o <out.mkv> <file1.mkv> <file2.mkv> ...")
	}
	GuardOverwrite(outPath)

	err := matroska.Join(context.Background(), sources, outPath, matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	fmt.Printf("joined %d files → %s\n", len(sources), outPath)
}
