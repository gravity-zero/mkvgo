package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mp4"
)

// CmdKeyframes prints a file's video keyframe timestamps (ms). MKV/WebM reads the
// Cues seek index head-only; MP4 builds the sample table (opt-in). A Cues-less
// Matroska builds the complete keyframe index from a sequential structural pass.
func CmdKeyframes(path string) {
	var fs *mkv.FS
	if isRemoteURL(path) {
		fs = remotePort(path) // head-only over HTTP Range requests / s3 SigV4
	}
	var ks []int64
	if isMP4Path(path) {
		c, _, err := mp4.OpenMeta(context.Background(), path, mp4.Options{Keyframes: true, FS: fs})
		if err != nil {
			Fatal(err.Error())
		}
		ks = c.Keyframes
	} else {
		// WithKeyframeIndex is a no-op when the file has Cues, so passing it always
		// keeps Cues-indexed files head-only while building the complete index for
		// Cues-less ones (the "no external fallback" path). A nil fs is the OS.
		c, err := matroska.OpenMetaWithFS(context.Background(), path, fs, matroska.WithKeyframeIndex())
		if err != nil {
			Fatal(err.Error())
		}
		ks = c.Keyframes
	}

	if JsonOutput {
		if ks == nil {
			ks = []int64{}
		}
		PrintJSON(ks)
		return
	}
	if len(ks) == 0 {
		fmt.Println("No keyframe index available (no Cues index / no video track).")
		return
	}
	for _, ms := range ks {
		fmt.Printf("%s  %d\n", FmtMs(ms), ms)
	}
}

// CmdToVTT converts an external subtitle sidecar (.srt/.ass/.ssa/.vtt) to WebVTT.
func CmdToVTT(args []string) {
	var outPath string
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-o" {
			i++
			if i < len(args) {
				outPath = args[i]
			}
			continue
		}
		rejectFlagArg(args[i])
		rest = append(rest, args[i])
	}
	if len(rest) < 1 || outPath == "" {
		Fatal("usage: " + CmdUsage["to-vtt"])
	}
	src := rest[0]
	GuardOverwrite(outPath)

	out, err := os.Create(outPath)
	if err != nil {
		Fatal(err.Error())
	}
	defer out.Close()
	if err := matroska.SubtitleFileToWebVTT(src, out); err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("converted %s → %s\n", src, outPath)
}

// CmdExtractFrame extracts the video keyframe nearest a timestamp as a
// decoder-ready file (Annex-B or IVF) - the mkvgo half of a thumbnail
// pipeline; feed it to any decoder for a thumbnail.
func CmdExtractFrame(args []string) {
	var outPath string
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-o" {
			i++
			if i < len(args) {
				outPath = args[i]
			}
			continue
		}
		rejectFlagArg(args[i])
		rest = append(rest, args[i])
	}
	if len(rest) < 2 || outPath == "" {
		Fatal("usage: " + CmdUsage["extract-frame"])
	}
	atMs, err := ParseTimePoint(rest[1])
	if err != nil {
		Fatal(err.Error())
	}
	ks, err := matroska.ExtractKeyframeSample(context.Background(), rest[0], atMs)
	if err != nil {
		Fatal(err.Error())
	}
	GuardOverwrite(outPath)
	if err := os.WriteFile(outPath, ks.Data, 0o644); err != nil {
		Fatal(err.Error())
	}
	fmt.Fprintf(os.Stderr, "keyframe @ %s (%s) → %s (%d bytes, decoder-ready)\n",
		FmtMs(ks.PtsMs), ks.Codec, outPath, len(ks.Data))
}
