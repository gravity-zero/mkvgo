package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mp4"
)

func CmdSplit(args []string) {
	if len(args) < 3 {
		Fatal("usage: " + CmdUsage["split"])
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

	so := matroska.SplitOptions{
		SourcePath: source,
		OutputDir:  outDir,
		ByChapters: byChapters,
		Ranges:     ranges,
		EveryMs:    everyMs,
		Pattern:    pattern,
	}
	var outputs []string
	var err error
	if isMP4Content(source) {
		guardMP4Output(pattern)
		outputs, err = mp4.Split(context.Background(), so, mp4Options())
	} else {
		outputs, err = matroska.Split(context.Background(), so, matroska.Options{Progress: NewProgressBar()})
	}
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
		Fatal("usage: " + CmdUsage["join"])
	}
	var outPath string
	var sources []string

	fl := newFlags("join")
	fl.String(&outPath, "-o")
	sources = fl.Parse(args)
	if outPath == "" || len(sources) == 0 {
		Fatal("usage: " + CmdUsage["join"])
	}
	GuardOverwrite(outPath)

	var err error
	if allMP4(outPath, sources...) {
		err = mp4.Join(context.Background(), sources, outPath, mp4Options())
	} else {
		err = matroska.Join(context.Background(), sources, outPath, matroska.Options{Progress: NewProgressBar()})
	}
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	fmt.Printf("joined %d files → %s\n", len(sources), outPath)
}

// allMP4 reports whether every input is an MP4 (then outPath must not carry a Matroska name), refusing a mix of containers.
func allMP4(outPath string, inputs ...string) bool {
	guardDistinct(outPath, inputs...)
	mp4s := 0
	for _, in := range inputs {
		if _, err := os.Stat(in); err != nil {
			FatalErr(err)
		}
		if isMP4Content(in) {
			mp4s++
		}
	}
	switch mp4s {
	case 0:
		return false
	case len(inputs):
		guardMP4Output(outPath)
		return true
	}
	Fatal("the inputs mix MP4 and Matroska files: convert them to one container first (from-mp4 or to-mp4)")
	return false
}
