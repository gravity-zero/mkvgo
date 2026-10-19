package commands

import (
	"context"
	"fmt"
	"strconv"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mp4"
)

func CmdDemux(args []string) {
	if len(args) < 3 {
		Fatal("usage: mkvgo demux <file.mkv> -o <dir> [-t trackID,...]")
	}
	source := args[0]
	var outDir string
	var trackIDs []uint64

	fl := newFlags("demux")
	fl.String(&outDir, "-o")
	fl.Func(func(v string) { trackIDs = ParseTrackIDs(v) }, "-t")
	fl.Parse(args[1:])
	if outDir == "" {
		Fatal("missing -o <dir>")
	}

	err := matroska.Demux(context.Background(), matroska.DemuxOptions{
		SourcePath: source, OutputDir: outDir, TrackIDs: trackIDs, NoOverwrite: !Force,
	}, matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	fmt.Printf("demuxed %s → %s\n", source, outDir)
}

func CmdMux(args []string) {
	if len(args) < 3 {
		Fatal("usage: " + CmdUsage["mux"])
	}
	var outPath string
	var inputs []matroska.TrackInput

	fl := newFlags("mux")
	fl.String(&outPath, "-o")
	for _, spec := range fl.Parse(args) {
		srcPath, trackID, ok := splitTrackSpec(spec)
		if !ok {
			Fatal(fmt.Sprintf("invalid track spec %q, expected file:trackID", spec))
		}
		id, err := strconv.ParseUint(trackID, 10, 64)
		if err != nil {
			Fatal(fmt.Sprintf("invalid track ID %q: %v", trackID, err))
		}
		inputs = append(inputs, matroska.TrackInput{
			SourcePath: srcPath, TrackID: id, IsDefault: true,
		})
	}
	if outPath == "" {
		Fatal("missing -o <out.mkv>")
	}
	if len(inputs) == 0 {
		Fatal("no track inputs specified")
	}
	GuardOverwrite(outPath)

	var paths []string
	for _, in := range inputs {
		paths = append(paths, in.SourcePath)
	}
	mo := matroska.MuxOptions{OutputPath: outPath, Tracks: inputs}
	var err error
	if allMP4(outPath, paths...) {
		err = mp4.Mux(context.Background(), mo, mp4Options())
	} else {
		err = matroska.Mux(context.Background(), mo, matroska.Options{Progress: NewProgressBar()})
	}
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	fmt.Printf("muxed %d tracks → %s\n", len(inputs), outPath)
}

func CmdRemoveTrack(args []string) {
	if len(args) < 5 {
		Fatal("usage: " + CmdUsage["remove-track"])
	}
	source := args[0]
	var outPath string
	var trackIDs []uint64

	fl := newFlags("remove-track")
	fl.String(&outPath, "-o")
	fl.Func(func(v string) { trackIDs = ParseTrackIDs(v) }, "-t")
	fl.Parse(args[1:])
	if outPath == "" || len(trackIDs) == 0 {
		Fatal("usage: " + CmdUsage["remove-track"])
	}
	GuardOverwrite(outPath)

	guardDistinct(outPath, source)
	var err error
	if isMP4Content(source) {
		guardMP4Output(outPath)
		err = mp4.RemoveTrack(context.Background(), source, outPath, trackIDs, mp4Options())
	} else {
		err = matroska.RemoveTrack(context.Background(), source, outPath, trackIDs, matroska.Options{Progress: NewProgressBar()})
	}
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	fmt.Printf("removed tracks %v → %s\n", trackIDs, outPath)
}

func CmdAddTrack(args []string) {
	if len(args) < 4 {
		Fatal("usage: " + CmdUsage["add-track"])
	}
	base := args[0]
	var outPath string
	var input matroska.TrackInput

	fl := newFlags("add-track")
	fl.String(&outPath, "-o")
	fl.Func(func(v string) { input.Language = v }, "-lang")
	fl.Func(func(v string) { input.Name = v }, "-name")
	for _, op := range fl.Parse(args[1:]) {
		if srcPath, trackID, ok := splitTrackSpec(op); ok {
			id, err := strconv.ParseUint(trackID, 10, 64)
			if err != nil {
				Fatal(fmt.Sprintf("invalid track ID %q", trackID))
			}
			input.SourcePath = srcPath
			input.TrackID = id
		}
	}
	if outPath == "" || input.SourcePath == "" {
		Fatal("usage: " + CmdUsage["add-track"])
	}
	guardOutput(outPath, base, input.SourcePath)

	var err error
	if allMP4(outPath, base, input.SourcePath) {
		err = mp4.AddTrack(context.Background(), base, outPath, input, mp4Options())
	} else {
		err = matroska.AddTrack(context.Background(), base, outPath, input, matroska.Options{Progress: NewProgressBar()})
	}
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	fmt.Printf("added track %s:%d → %s\n", input.SourcePath, input.TrackID, outPath)
}
