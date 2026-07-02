package commands

import (
	"context"
	"fmt"
	"os"
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
		FS:        sourceFS(src),
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
	fmt.Printf("HLS written → %s (play master.m3u8; init.mp4 + segments + subtitle renditions)\n", outDir)
}

// CmdHLSSegment serves one resource of an on-demand HLS plan: the master or
// media playlist, the init segment, or the n-th media segment — built by
// reading only that segment's window from the source (seeked through the
// Cues), so a server can answer HLS requests with no pre-generated files.
// The source may be a local path or an http(s) URL.
func CmdHLSSegment(args []string) {
	var outPath string
	var segMs int64
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			if i < len(args) {
				outPath = args[i]
			}
		case "-segment", "--segment":
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
	if len(rest) < 2 {
		Fatal("usage: " + CmdUsage["hls-segment"])
	}
	src, what := rest[0], rest[1]

	plan, err := mp4.PlanHLS(context.Background(), src, mp4.Options{
		FS:        sourceFS(src),
		SegmentMs: segMs,
		OnDrop: func(d mp4.DroppedTrack) {
			fmt.Fprintf(os.Stderr, "dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
		},
	})
	if err != nil {
		Fatal(err.Error())
	}

	var data []byte
	switch what {
	case "master":
		data = plan.MasterPlaylist()
	case "playlist":
		data = plan.MediaPlaylist()
	case "init":
		data = plan.InitSegment()
	default:
		n, err := strconv.Atoi(what)
		if err != nil || n < 1 || n > plan.NumSegments() {
			Fatal(fmt.Sprintf("segment %q out of range (1..%d, or master/playlist/init)", what, plan.NumSegments()))
		}
		data, err = plan.Segment(context.Background(), n-1)
		if err != nil {
			Fatal(err.Error())
		}
	}

	if outPath == "" {
		if _, err := os.Stdout.Write(data); err != nil {
			Fatal(err.Error())
		}
		return
	}
	GuardOverwrite(outPath)
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		Fatal(err.Error())
	}
	fmt.Fprintf(os.Stderr, "%s → %s (%d bytes, %d segments total)\n", what, outPath, len(data), plan.NumSegments())
}
