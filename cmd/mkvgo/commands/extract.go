package commands

import (
	"context"
	"fmt"
	"strconv"

	"github.com/gravity-zero/mkvgo/matroska"
)

func CmdExtractAttachment(args []string) {
	if len(args) < 4 {
		Fatal("usage: mkvgo extract-attachment <file.mkv> <attachmentID> -o <outfile>")
	}
	source := args[0]
	attID, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		Fatal(fmt.Sprintf("invalid attachment ID %q", args[1]))
	}
	var outPath string
	for i := 2; i < len(args); i++ {
		if args[i] == "-o" {
			i++
			outPath = args[i]
		}
	}
	if outPath == "" {
		Fatal("usage: mkvgo extract-attachment <file.mkv> <attachmentID> -o <outfile>")
	}

	err = matroska.ExtractAttachment(context.Background(), source, attID, outPath)
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("extracted attachment #%d → %s\n", attID, outPath)
}

func CmdExtractSubtitle(args []string) {
	if len(args) < 5 {
		Fatal("usage: mkvgo extract-subtitle <file.mkv> -t <trackID> -o <out> [-format srt|ass]")
	}
	source := args[0]
	var outPath, format string
	var trackID uint64
	format = "srt"

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			outPath = args[i]
		case "-t":
			i++
			id, err := strconv.ParseUint(args[i], 10, 64)
			if err != nil {
				Fatal(fmt.Sprintf("invalid track ID %q", args[i]))
			}
			trackID = id
		case "-format":
			i++
			format = args[i]
		}
	}
	if outPath == "" || trackID == 0 {
		Fatal("usage: mkvgo extract-subtitle <file.mkv> -t <trackID> -o <out> [-format srt|ass]")
	}

	var err error
	switch format {
	case "srt":
		err = matroska.ExtractSubtitle(context.Background(), source, trackID, outPath)
	case "ass", "ssa":
		err = matroska.ExtractASS(context.Background(), source, trackID, outPath)
	default:
		Fatal(fmt.Sprintf("unknown format %q (supported: srt, ass)", format))
	}
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("extracted subtitle track %d (%s) → %s\n", trackID, format, outPath)
}
