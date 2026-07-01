package commands

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mp4"
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
		} else {
			rejectFlagArg(args[i])
		}
	}
	if outPath == "" {
		Fatal("usage: mkvgo extract-attachment <file.mkv> <attachmentID> -o <outfile>")
	}
	GuardOverwrite(outPath)

	err = matroska.ExtractAttachment(context.Background(), source, attID, outPath)
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("extracted attachment #%d → %s\n", attID, outPath)
}

func CmdExtractSubtitle(args []string) {
	usage := CmdUsage["extract-subtitle"]
	if len(args) < 5 {
		Fatal("usage: " + usage)
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
		default:
			rejectFlagArg(args[i])
		}
	}
	if outPath == "" || trackID == 0 {
		Fatal("usage: " + usage)
	}
	GuardOverwrite(outPath)

	var err error
	switch format {
	case "vtt":
		err = extractWebVTT(source, trackID, outPath)
	case "srt":
		if isMP4Path(source) {
			Fatal("MP4 subtitle extraction supports only -format vtt")
		}
		err = matroska.ExtractSubtitle(context.Background(), source, trackID, outPath)
	case "ass", "ssa":
		if isMP4Path(source) {
			Fatal("MP4 subtitle extraction supports only -format vtt")
		}
		err = matroska.ExtractASS(context.Background(), source, trackID, outPath)
	default:
		Fatal(fmt.Sprintf("unknown format %q (supported: srt, ass, vtt)", format))
	}
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("extracted subtitle track %d (%s) → %s\n", trackID, format, outPath)
}

// extractWebVTT writes subtitle track trackID of source (MKV/WebM or MP4) to
// outPath as WebVTT.
func extractWebVTT(source string, trackID uint64, outPath string) error {
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	if isMP4Path(source) {
		return mp4.ExtractSubtitleWebVTT(context.Background(), source, trackID, out)
	}
	return matroska.ExtractSubtitleWebVTT(context.Background(), source, trackID, out)
}
