package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
)

// CmdIngest implements `mkvgo ingest`: the one-call onboarding decision for a
// media server - direct-play, remux to on-demand HLS (optionally patching in
// a seek index first), or recommend an ABR ladder for an external transcode.
// No decode, no transcode: mkvgo only decides and, with -reindex, performs
// the one repair a remux decision may call for.
func CmdIngest(args []string) {
	targetName := "mse-generic"
	reindex := false
	analyze := false
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-target", "--target":
			i++
			if i >= len(args) {
				Fatal("usage: " + CmdUsage["ingest"])
			}
			targetName = args[i]
		case "-reindex", "--reindex":
			reindex = true
		case "-analyze", "--analyze":
			analyze = true
		default:
			rejectFlagArg(a)
			rest = append(rest, a)
		}
	}
	if len(rest) < 1 {
		Fatal("usage: " + CmdUsage["ingest"])
	}
	path := rest[0]

	opts := matroska.IngestOptions{
		Target:          targetName,
		Reindex:         reindex,
		IncludeAnalysis: analyze,
	}
	if isRemoteURL(path) {
		opts.FS = remotePort(path)
	}

	plan, err := matroska.Ingest(context.Background(), path, opts)
	if err != nil {
		Fatal(err.Error())
	}

	if JsonOutput {
		PrintJSON(plan)
		return
	}
	printServingPlan(plan)
}

func printServingPlan(p *matroska.ServingPlan) {
	fmt.Printf("Target: %s\n", p.Target)
	fmt.Printf("Source container: %s\n", p.SourceContainer)
	fmt.Printf("Strategy: %s\n", p.Strategy)
	if p.RemuxContainer != "" {
		fmt.Printf("Remux container: %s\n", p.RemuxContainer)
	}
	if p.Strategy == matroska.StrategyRemuxHLS {
		fmt.Printf("Seek index present: %v\n", p.HasSeekIndex)
		if p.NeedsReindex {
			fmt.Println("Reindex needed: yes")
		}
		if p.Reindexed {
			fmt.Println("Reindexed in place: yes")
		}
		if p.NeedsReindex && !p.Reindexed {
			fmt.Printf("In-place reindex possible: %v\n", p.ReindexInPlacePossible)
		}
	}
	if p.Strategy == matroska.StrategyTranscode && len(p.Ladder) > 0 {
		fmt.Println("Recommended ladder:")
		for _, r := range p.Ladder {
			fmt.Printf("  %-6s  %5dx%-5d  %6d kb/s\n", r.Label, r.Width, r.Height, r.BitrateKbps)
		}
	}
	if len(p.Reasons) > 0 {
		fmt.Println("Reasons:")
		for _, r := range p.Reasons {
			fmt.Println("  - " + r)
		}
	}
	if p.Analysis != nil {
		fmt.Printf("Analysis: %d clusters, %d blocks, duration %s\n",
			p.Analysis.ClusterCount, p.Analysis.BlockCount, FmtMs(p.Analysis.DurationMs))
	}
}
