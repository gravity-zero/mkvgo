package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/gravity-zero/mkvgo/matroska"
)

func CmdMerge(args []string) {
	if len(args) < 3 {
		Fatal("usage: mkvgo merge -o <out.mkv> <file1.mkv> [<file2.mkv> ...]")
	}
	var outPath string
	var inputs []matroska.MergeInput

	for i := 0; i < len(args); i++ {
		if args[i] == "-o" {
			i++
			if i >= len(args) {
				Fatal("-o needs a value")
			}
			outPath = args[i]
			continue
		}
		rejectFlagArg(args[i])
		inputs = append(inputs, matroska.MergeInput{SourcePath: args[i]})
	}
	if outPath == "" || len(inputs) == 0 {
		Fatal("usage: mkvgo merge -o <out.mkv> <file1.mkv> [<file2.mkv> ...]")
	}
	GuardOverwrite(outPath)

	err := matroska.Merge(context.Background(), matroska.MergeOptions{
		OutputPath: outPath, Inputs: inputs,
	}, matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("merged %d sources → %s\n", len(inputs), outPath)
}

func CmdMergeSubtitle(args []string) {
	if len(args) < 4 {
		Fatal("usage: mkvgo merge-subtitle <file.mkv> -o <out.mkv> <subtitle> [-format srt|ass] [-lang code] [-name text]")
	}
	source := args[0]
	var outPath, subPath, format, lang, name string
	lang = "und"

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			if i >= len(args) {
				Fatal("-o needs a value")
			}
			outPath = args[i]
		case "-format":
			i++
			if i >= len(args) {
				Fatal("-format needs a value")
			}
			format = args[i]
		case "-lang":
			i++
			if i >= len(args) {
				Fatal("-lang needs a value")
			}
			lang = args[i]
		case "-name":
			i++
			if i >= len(args) {
				Fatal("-name needs a value")
			}
			name = args[i]
		default:
			rejectFlagArg(args[i])
			subPath = args[i]
		}
	}
	if outPath == "" || subPath == "" {
		Fatal("usage: mkvgo merge-subtitle <file.mkv> -o <out.mkv> <subtitle> [-format srt|ass]")
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
		Fatal(err.Error())
	}
	fmt.Printf("merged %s subtitle → %s\n", format, outPath)
}
