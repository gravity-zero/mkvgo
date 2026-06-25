package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mp4"
)

// CmdKeyframes prints a file's video keyframe timestamps (ms). MKV/WebM reads the
// Cues seek index head-only; MP4 builds the sample table (opt-in). A Cues-less
// Matroska falls back to a bounded sampled index (Cluster timestamps).
func CmdKeyframes(path string) {
	var ks []int64
	if isMP4Path(path) {
		c, _, err := mp4.OpenMeta(context.Background(), path, mp4.Options{Keyframes: true})
		if err != nil {
			Fatal(err.Error())
		}
		ks = c.Keyframes
	} else {
		// WithSampledKeyframes is a no-op when the file has Cues, so passing it
		// always keeps Cues-indexed files head-only while recovering an index for
		// Cues-less ones.
		c, err := matroska.OpenMeta(context.Background(), path, matroska.WithSampledKeyframes(0))
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
		rest = append(rest, args[i])
	}
	if len(rest) < 1 || outPath == "" {
		Fatal("usage: " + CmdUsage["to-vtt"])
	}
	src := rest[0]

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
