package commands

import (
	"context"
	"fmt"
	"strconv"

	"github.com/gravity-zero/mkvgo/mp4"
)

// CmdToHLS remuxes an MKV/WebM file to a fragmented-MP4 HLS presentation
// (init.mp4 + segments + playlist.m3u8) in an output directory.
func CmdToHLS(args []string) {
	var outDir string
	var segMs int64
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			if i < len(args) {
				outDir = args[i]
			}
		case "-segment", "--segment":
			// Segment length in seconds (HLS convention, like ffmpeg -hls_time).
			i++
			if i < len(args) {
				secs, err := strconv.ParseFloat(args[i], 64)
				if err != nil || secs <= 0 {
					Fatal(fmt.Sprintf("invalid -segment duration %q (seconds)", args[i]))
				}
				segMs = int64(secs * 1000)
			}
		default:
			rejectFlagArg(args[i])
			rest = append(rest, args[i])
		}
	}
	if len(rest) < 1 || outDir == "" {
		Fatal("usage: " + CmdUsage["to-hls"])
	}
	src := rest[0]

	err := mp4.RemuxToHLS(context.Background(), src, outDir, mp4.Options{
		Progress:  NewProgressBar(),
		SegmentMs: segMs,
		OnDrop: func(d mp4.DroppedTrack) {
			fmt.Printf("dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
		},
	})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("HLS written → %s (init.mp4 + segments + playlist.m3u8)\n", outDir)
}
