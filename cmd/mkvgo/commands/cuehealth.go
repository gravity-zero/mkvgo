package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/gravity-zero/mkvgo/matroska"
)

const cueHealthUsage = "usage: mkvgo cue-health <file.mkv> [-json] [-probe]"

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
	var jsonOut, probe bool
	var rest []string
	for _, a := range args {
		switch a {
		case "-json", "--json":
			jsonOut = true
		case "-probe", "--probe":
			probe = true
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
	// -probe: look inside each hole (bounded, header-only) so the printout
	// says what a reindex would find there - the same pass diagnose runs.
	if probe {
		if err := matroska.ProbeCueHoles(context.Background(), rest[0], report); err != nil {
			Fatal(err.Error())
		}
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
			fmt.Printf("  video coverage: worst hole %s at %s\n", FmtMs(report.MaxVideoGapMs), FmtMs(report.MaxVideoGapAtMs))
			end := "declared end"
			if report.VideoEndExact {
				end = "picture ends"
			}
			fmt.Printf("  %s %s, %s past the last video cue\n", end, FmtMs(report.VideoEndMs), FmtMs(report.TailGapMs))
			if report.VideoShortfallMs > 0 {
				fmt.Printf("  picture missing from the stream: about %s (stated frames vs duration at frame rate)\n", FmtMs(report.VideoShortfallMs))
			}
			for _, h := range report.Holes {
				line := fmt.Sprintf("  hole: %s at %s", FmtMs(h.GapMs), FmtMs(h.AtMs))
				switch h.Content {
				case "uncued-keyframes":
					line += " - holds uncued keyframes (a reindex closes it)"
				case "no-keyframes":
					line += fmt.Sprintf(" - %d video frame(s), no keyframe, none for %s of it (only a re-encode makes it seekable)", h.VideoBlocks, FmtMs(h.VideoAbsentMs))
				case "picture-missing":
					line += fmt.Sprintf(" - no video at all for %s of it (the picture is missing from the stream)", FmtMs(h.VideoAbsentMs))
				default:
					if probe {
						line += " - not probed (stale cue position or truncated walk)"
					}
				}
				fmt.Println(line)
			}
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
