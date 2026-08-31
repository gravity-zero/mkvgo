package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
)

// CmdAnalyze prints a file's stream statistics: exact per-track frame/keyframe
// counts (lacing expanded), byte totals, average/peak bitrate, GOP spans and a
// declared-vs-true duration reconciliation - computed from block headers
// alone, a fast structural pass that never decodes a sample. MP4 is not
// supported yet (Matroska/WebM only) - see docs/library.md.
func CmdAnalyze(args []string) {
	var rest []string
	for _, a := range args {
		rejectFlagArg(a)
		rest = append(rest, a)
	}
	if len(rest) < 1 {
		Fatal("usage: " + CmdUsage["analyze"])
	}
	path := rest[0]
	if isMP4Path(path) {
		Fatal("analyze supports Matroska/WebM only for now (MP4 is a follow-up - see docs/library.md)")
	}

	var opts []matroska.Options
	if isRemoteURL(path) {
		opts = append(opts, matroska.Options{FS: remotePort(path)})
	}

	report, err := matroska.Analyze(context.Background(), path, opts...)
	if err != nil {
		Fatal(err.Error())
	}

	if JsonOutput {
		PrintJSON(report)
		return
	}
	printAnalyzeReport(report)
}

func printAnalyzeReport(r *matroska.AnalyzeReport) {
	fmt.Printf("Duration: %s (declared %s)\n", FmtMs(r.DurationMs), FmtMs(r.DeclaredDurationMs))
	fmt.Printf("Overall bitrate: %d kb/s\n", r.OverallBitrateBps/1000)
	fmt.Printf("Clusters: %d, blocks: %d\n", r.ClusterCount, r.BlockCount)

	for _, ts := range r.Tracks {
		fmt.Printf("\nTrack %d (%s, %s): %d frames (%d packets), %d keyframes, %d bytes\n",
			ts.TrackID, ts.Type, ts.Codec, ts.Frames, ts.Packets, ts.Keyframes, ts.Bytes)
		fps := fmt.Sprintf("%.3f fps", ts.FrameRateAvg)
		if ts.FrameRateMode != "" {
			fps += " " + ts.FrameRateMode
		}
		fmt.Printf("  avg %d kb/s, peak %d kb/s, duration %s, %s\n",
			ts.AvgBitrateBps/1000, ts.PeakBitrateBps/1000, FmtMs(ts.DurationMs), fps)
		if ts.Type == string(mkv.VideoTrack) && ts.MaxGopFrames > 0 {
			fmt.Printf("  GOP: min %d, max %d, avg %.1f frames; keyframe every ~%dms (max %dms at %s); reordered=%v\n",
				ts.MinGopFrames, ts.MaxGopFrames, ts.AvgGopFrames, ts.KeyframeEveryMsAvg,
				ts.MaxKeyframeGapMs, FmtMs(ts.MaxKeyframeGapAtMs), ts.Reordered)
		}
	}

	if len(r.Warnings) > 0 {
		fmt.Println("\nWarnings:")
		for _, w := range r.Warnings {
			fmt.Println("  - " + w)
		}
	}
}
