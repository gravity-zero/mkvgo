package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/gravity-zero/mkvgo/matroska"
)

const cueHealthUsage = "usage: mkvgo cue-health <file.mkv> [-json]"

// CmdCueHealth classifies the seek index head-only and judges whether it can
// actually seek video: it spots the dormant defect where a video file's index
// exists but keys on audio - readers call the file "indexed", every seek lands
// mid-GOP - and the index too coarse to land anywhere near its target, in
// milliseconds, without walking the file (validate proves cue times against
// real keyframes but reads everything). Cues on other tracks are counted and
// reported, never held against a file: seeking never uses them.
//
// Exit contract: 0 healthy, 1 unhealthy (scriptable, like validate).
func CmdCueHealth(args []string) {
	var jsonOut bool
	var rest []string
	for _, a := range args {
		switch a {
		case "-json", "--json":
			jsonOut = true
		default:
			rejectFlagArg(a)
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		Fatal(cueHealthUsage)
	}

	report, err := matroska.CueHealth(context.Background(), rest[0])
	if err != nil {
		Fatal(err.Error())
	}

	if jsonOut || JsonOutput {
		PrintJSON(report)
	} else {
		fmt.Printf("%s: %d cue(s)", rest[0], report.TotalCues)
		if report.TotalCues > 0 {
			fmt.Printf(" (%d video, %d non-video, %d unknown-track), %s to %s",
				report.VideoCues, report.NonVideoCues, report.UnknownTrackCues,
				FmtMs(report.FirstCueMs), FmtMs(report.LastCueMs))
		}
		fmt.Println()
		if report.VideoCues > 0 {
			fmt.Printf("  video coverage: worst hole %s\n", FmtMs(report.MaxVideoGapMs))
		}
		if report.Healthy {
			fmt.Println("  index healthy")
		} else {
			fmt.Printf("  index UNHEALTHY: %s\n", report.Reason)
		}
	}
	if !report.Healthy {
		os.Exit(1)
	}
}
