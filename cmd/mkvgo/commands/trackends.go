package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
)

const trackEndsUsage = "usage: mkvgo track-ends <file.mkv> [-json]"

// CmdTrackEnds prints where each track's content really ends - the declared
// duration is only the longest track's end - from the statistics tags when
// they describe the file, else a bounded header-only walk of the tail: the
// picture's end and how far any audio track stops before it (the defect a
// structurally healthy file hides: playlists promising audio that never comes).
func CmdTrackEnds(args []string) {
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
		Fatal(trackEndsUsage)
	}
	report, err := matroska.TrackEnds(context.Background(), rest[0])
	if err != nil {
		Fatal(err.Error())
	}
	if jsonOut || JsonOutput {
		PrintJSON(report)
		return
	}
	fmt.Printf("%s: declared duration %s\n", rest[0], FmtMs(report.DeclaredDurationMs))
	for _, e := range report.Ends {
		switch e.Source {
		case "":
			fmt.Printf("  track %d (%s): end unknown (never seen)\n", e.Track, e.Type)
		case "walk-bound":
			fmt.Printf("  track %d (%s): ends at or before %s (silent through the tail walked)\n", e.Track, e.Type, FmtMs(e.EndMs))
		default:
			fmt.Printf("  track %d (%s): ends %s (%s)\n", e.Track, e.Type, FmtMs(e.EndMs), e.Source)
		}
	}
	if report.VideoEndMs > 0 {
		fmt.Printf("  picture ends %s\n", FmtMs(report.VideoEndMs))
	}
	if report.AudioShortfallMs > 0 {
		fmt.Printf("  audio track %d stops %.3fs before the picture\n", report.ShortAudioTrack, float64(report.AudioShortfallMs)/1000)
	}
}
