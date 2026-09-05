package commands

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

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
			if i >= len(args) {
				Fatal("-o needs a value")
			}
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
	var outPath, format, indexPath string
	var trackID uint64
	format = "srt"

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			if i >= len(args) {
				Fatal("-o needs a value")
			}
			outPath = args[i]
		case "-index":
			i++
			if i >= len(args) {
				Fatal("-index needs a value")
			}
			indexPath = args[i]
		case "-t":
			i++
			if i >= len(args) {
				Fatal("-t needs a value")
			}
			id, err := strconv.ParseUint(args[i], 10, 64)
			if err != nil {
				Fatal(fmt.Sprintf("invalid track ID %q", args[i]))
			}
			trackID = id
		case "-format":
			i++
			if i >= len(args) {
				Fatal("-format needs a value")
			}
			format = args[i]
		default:
			rejectFlagArg(args[i])
		}
	}
	if outPath == "" || trackID == 0 {
		Fatal("usage: " + usage)
	}
	GuardOverwrite(outPath)

	if indexPath != "" {
		if format != "vtt" {
			Fatal("-index applies to -format vtt only")
		}
		if isMP4Path(source) {
			Fatal("-index applies to MKV/WebM only: an MP4 already carries its own sample table")
		}
		if err := extractWebVTTFromIndex(source, trackID, indexPath, outPath); err != nil {
			Fatal(err.Error())
		}
		fmt.Printf("extracted subtitle track %d (vtt, from %s) → %s\n", trackID, indexPath, outPath)
		return
	}

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

// extractWebVTTFromIndex serves one track from an index file written by
// CmdSubtitleIndex, instead of walking the source.
func extractWebVTTFromIndex(source string, trackID uint64, indexPath, outPath string) error {
	blob, err := os.ReadFile(indexPath)
	if err != nil {
		return err
	}
	var ix matroska.SubtitleIndex
	if err := ix.UnmarshalBinary(blob); err != nil {
		return err
	}
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	return matroska.ExtractSubtitleWebVTTFrom(context.Background(), source, trackID, &ix, out)
}

// CmdSubtitleIndex builds the subtitle block index of an MKV/WebM and writes it
// out. Matroska's Cues index the video track only, so without this every
// subtitle extraction re-walks the whole file; with it, extraction seeks.
func CmdSubtitleIndex(args []string) {
	usage := CmdUsage["subtitle-index"]
	if len(args) < 3 {
		Fatal("usage: " + usage)
	}
	source := args[0]
	var outPath string
	var trackIDs []uint64
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			if i >= len(args) {
				Fatal("-o needs a value")
			}
			outPath = args[i]
		case "-t":
			i++
			if i >= len(args) {
				Fatal("-t needs a value")
			}
			for _, part := range strings.Split(args[i], ",") {
				id, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64)
				if err != nil {
					Fatal(fmt.Sprintf("invalid track ID %q", part))
				}
				trackIDs = append(trackIDs, id)
			}
		default:
			rejectFlagArg(args[i])
		}
	}
	if outPath == "" {
		Fatal("usage: " + usage)
	}
	GuardOverwrite(outPath)

	ix, err := matroska.BuildSubtitleIndex(context.Background(), source, trackIDs)
	if err != nil {
		Fatal(err.Error())
	}
	blob, err := ix.MarshalBinary()
	if err != nil {
		Fatal(err.Error())
	}
	if err := os.WriteFile(outPath, blob, 0o644); err != nil {
		Fatal(err.Error())
	}
	total := 0
	for _, id := range ix.Tracks() {
		total += ix.Blocks(id)
	}
	fmt.Printf("indexed %d blocks over %d subtitle track(s) → %s (%d bytes)\n",
		total, len(ix.Tracks()), outPath, len(blob))
}
