package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mp4"
)

func CmdMerge(args []string) {
	if len(args) < 3 {
		Fatal("usage: " + CmdUsage["merge"])
	}
	var outPath string
	var inputs []matroska.MergeInput

	fl := newFlags("merge")
	fl.String(&outPath, "-o")
	for _, src := range fl.Parse(args) {
		inputs = append(inputs, matroska.MergeInput{SourcePath: src})
	}
	if outPath == "" || len(inputs) == 0 {
		Fatal("usage: " + CmdUsage["merge"])
	}
	GuardOverwrite(outPath)

	var paths []string
	for _, in := range inputs {
		paths = append(paths, in.SourcePath)
	}
	mo := matroska.MergeOptions{OutputPath: outPath, Inputs: inputs}
	var err error
	if allMP4(outPath, paths...) {
		err = mp4.Merge(context.Background(), mo, mp4Options())
	} else {
		err = matroska.Merge(context.Background(), mo, matroska.Options{Progress: NewProgressBar()})
	}
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	fmt.Printf("merged %d sources → %s\n", len(inputs), outPath)
}

func CmdMergeSubtitle(args []string) {
	if len(args) < 4 {
		Fatal("usage: " + CmdUsage["merge-subtitle"])
	}
	source := args[0]
	var outPath, subPath, format, lang, name string
	lang = "und"

	fl := newFlags("merge-subtitle")
	fl.String(&outPath, "-o")
	fl.String(&format, "-format")
	fl.String(&lang, "-lang")
	fl.String(&name, "-name")
	if ops := fl.Parse(args[1:]); len(ops) > 0 {
		subPath = ops[len(ops)-1]
	}
	if outPath == "" || subPath == "" {
		Fatal("usage: " + CmdUsage["merge-subtitle"])
	}
	GuardOverwrite(outPath)

	if format == "" {
		lower := strings.ToLower(subPath)
		switch {
		case strings.HasSuffix(lower, ".ass"), strings.HasSuffix(lower, ".ssa"):
			format = "ass"
		default:
			format = "srt"
		}
	}

	guardDistinct(outPath, source, subPath)
	if isMP4Content(source) {
		guardMP4Output(outPath)
		if format != "srt" {
			Fatal("an MP4 carries subtitles as plain timed text: merge an SRT file (styled ASS/SSA has no MP4 form; convert it to SRT first)")
		}
		err := mp4.MergeSubtitle(context.Background(), source, subPath, outPath, lang, name, mp4Options())
		ClearProgress()
		if err != nil {
			FatalErr(err)
		}
		fmt.Printf("merged %s subtitle → %s\n", format, outPath)
		return
	}
	opts := matroska.Options{Progress: NewProgressBar()}
	var err error
	switch format {
	case "srt":
		err = matroska.MergeSubtitle(context.Background(), source, subPath, outPath, lang, name, opts)
	case "ass", "ssa":
		err = matroska.MergeASS(context.Background(), source, subPath, outPath, lang, name, opts)
	default:
		Fatal(fmt.Sprintf("unknown format %q (supported: srt, ass)", format))
	}
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	fmt.Printf("merged %s subtitle → %s\n", format, outPath)
}
