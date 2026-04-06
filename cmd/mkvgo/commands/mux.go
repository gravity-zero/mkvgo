package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/matroska"
)

func CmdDemux(args []string) {
	if len(args) < 3 {
		Fatal("usage: mkvgo demux <file.mkv> -o <dir> [-t trackID,...]")
	}
	source := args[0]
	var outDir string
	var trackIDs []uint64

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			if i >= len(args) {
				Fatal("-o requires a directory")
			}
			outDir = args[i]
		case "-t":
			i++
			if i >= len(args) {
				Fatal("-t requires track IDs (comma-separated)")
			}
			trackIDs = ParseTrackIDs(args[i])
		}
	}
	if outDir == "" {
		Fatal("missing -o <dir>")
	}

	err := matroska.Demux(context.Background(), matroska.DemuxOptions{
		SourcePath: source, OutputDir: outDir, TrackIDs: trackIDs,
	})
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("demuxed %s → %s\n", source, outDir)
}

func CmdMux(args []string) {
	if len(args) < 3 {
		Fatal("usage: mkvgo mux -o <out.mkv> <file:trackID> [<file:trackID> ...]")
	}
	var outPath string
	var inputs []matroska.TrackInput

	for i := 0; i < len(args); i++ {
		if args[i] == "-o" {
			i++
			if i >= len(args) {
				Fatal("-o requires an output path")
			}
			outPath = args[i]
			continue
		}
		parts := strings.SplitN(args[i], ":", 2)
		if len(parts) != 2 {
			Fatal(fmt.Sprintf("invalid track spec %q, expected file:trackID", args[i]))
		}
		id, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			Fatal(fmt.Sprintf("invalid track ID %q: %v", parts[1], err))
		}
		inputs = append(inputs, matroska.TrackInput{
			SourcePath: parts[0], TrackID: id, IsDefault: true,
		})
	}
	if outPath == "" {
		Fatal("missing -o <out.mkv>")
	}
	if len(inputs) == 0 {
		Fatal("no track inputs specified")
	}

	err := matroska.Mux(context.Background(), matroska.MuxOptions{
		OutputPath: outPath, Tracks: inputs,
	})
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("muxed %d tracks → %s\n", len(inputs), outPath)
}

func CmdRemoveTrack(args []string) {
	if len(args) < 5 {
		Fatal("usage: mkvgo remove-track <file.mkv> -o <out.mkv> -t <trackID,...>")
	}
	source := args[0]
	var outPath string
	var trackIDs []uint64

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			outPath = args[i]
		case "-t":
			i++
			trackIDs = ParseTrackIDs(args[i])
		}
	}
	if outPath == "" || len(trackIDs) == 0 {
		Fatal("usage: mkvgo remove-track <file.mkv> -o <out.mkv> -t <trackID,...>")
	}

	err := matroska.RemoveTrack(context.Background(), source, outPath, trackIDs, matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("removed tracks %v → %s\n", trackIDs, outPath)
}

func CmdAddTrack(args []string) {
	if len(args) < 4 {
		Fatal("usage: mkvgo add-track <file.mkv> -o <out.mkv> <source:trackID> [-lang code] [-name text]")
	}
	base := args[0]
	var outPath string
	var input matroska.TrackInput

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			outPath = args[i]
		case "-lang":
			i++
			input.Language = args[i]
		case "-name":
			i++
			input.Name = args[i]
		default:
			parts := strings.SplitN(args[i], ":", 2)
			if len(parts) == 2 {
				id, err := strconv.ParseUint(parts[1], 10, 64)
				if err != nil {
					Fatal(fmt.Sprintf("invalid track ID %q", parts[1]))
				}
				input.SourcePath = parts[0]
				input.TrackID = id
			}
		}
	}
	if outPath == "" || input.SourcePath == "" {
		Fatal("usage: mkvgo add-track <file.mkv> -o <out.mkv> <source:trackID>")
	}

	err := matroska.AddTrack(context.Background(), base, outPath, input)
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("added track %s:%d → %s\n", input.SourcePath, input.TrackID, outPath)
}
