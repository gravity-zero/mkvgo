package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/gravity-zero/mkvgo/matroska"
)

const diagnoseUsage = "usage: mkvgo diagnose <file.mkv> [-json]"

// CmdDiagnose classifies a file in one call - seek-index health, per-track
// audio start delays, declared-size coherence, and (only when the size check
// suggests damage) the full tolerant walk - and names the remedy for every
// finding, so a scan can route each file straight to the right repair
// (reindex / retime / resync / re-download).
//
// Exit contract: 0 healthy, 1 findings present (scriptable, like validate).
func CmdDiagnose(args []string) {
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
		Fatal(diagnoseUsage)
	}

	d, err := matroska.Diagnose(context.Background(), rest[0])
	if err != nil {
		Fatal(err.Error())
	}

	if jsonOut || JsonOutput {
		PrintJSON(d)
	} else {
		if d.Healthy {
			fmt.Printf("%s: healthy\n", rest[0])
		} else {
			fmt.Printf("%s: %d finding(s)\n", rest[0], len(d.Findings))
			for _, f := range d.Findings {
				fmt.Printf("  [%s] %s\n      remedy: %s\n", f.Kind, f.Detail, f.Remedy)
			}
		}
	}
	if !d.Healthy {
		os.Exit(1)
	}
}
