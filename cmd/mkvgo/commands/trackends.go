package commands

import (
	"fmt"
	"os"

	"github.com/gravity-zero/mkvgo/matroska"
)

const trackEndsUsage = "usage: mkvgo track-ends <file.mkv> [-json]"

// CmdTrackEnds prints where each track's content really ends - the declared
// duration is only the longest track's end - from the statistics tags when
// they describe the file, else a bounded header-only walk of the tail: the
// picture's end and how far any audio track stops before it (the defect a
// structurally healthy file hides: playlists promising audio that never comes).
func CmdTrackEnds(args []string) {
	requireLocal("track-ends", args...)
	var jsonOut bool
	var rest []string
	fl := newFlags("CmdTrackEnds")
	fl.Bool(&jsonOut, "-json", "--json")
	rest = fl.Parse(args)
	if len(rest) != 1 {
		FatalUsage(trackEndsUsage)
	}
	report, err := matroska.TrackEnds(cliCtx, rest[0])
	if err != nil {
		FatalErr(err)
	}
	if cut := sourceCutShort(rest[0]); cut != nil {
		noteDamage()
		fmt.Fprintf(os.Stderr, "warning: %s is cut short (%d of %d bytes): the ends are those of what arrived\n", rest[0], cut.FileSize, cut.DeclaredSize)
	} else if report.StoppedShort {
		noteDamage()
		fmt.Fprintf(os.Stderr, "warning: %s stops inside its media (cut short): the ends are those of what arrived\n", rest[0])
	}
	if report.SkippedBytes > 0 {
		noteDamage()
	}
	if jsonOut || JsonOutput {
		PrintJSON(report)
		return
	}
	fmt.Printf("%s: declared duration %s\n", rest[0], FmtMs(report.DeclaredDurationMs))
	if report.StartMs > 0 {
		fmt.Printf("  timeline starts at %.3fs (the ends below are positions on it, not lengths)\n", float64(report.StartMs)/1000)
	}
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
	if report.SkippedBytes > 0 {
		fmt.Printf("  the walk skipped %d byte(s) past an element it cannot read inside the file, media included (damage map: mkvgo salvage --dry-run; repair: mkvgo reindex --resync)\n", report.SkippedBytes)
	}
	if report.AudioShortfallMs > 0 {
		fmt.Printf("  audio track %d stops %.3fs before the picture\n", report.ShortAudioTrack, float64(report.AudioShortfallMs)/1000)
	}
}
