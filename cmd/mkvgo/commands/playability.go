package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/gravity-zero/mkvgo/matroska"
)

// CmdPlayability implements `mkvgo playability`: per-track and overall
// direct-play/remux/transcode verdicts against a named target, from head-only
// metadata only (no decode, no external probe).
func CmdPlayability(args []string) {
	targetName := "mse-generic"
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-target", "--target":
			i++
			if i >= len(args) {
				Fatal("usage: " + CmdUsage["playability"])
			}
			targetName = args[i]
		default:
			rejectFlagArg(a)
			rest = append(rest, a)
		}
	}
	if len(rest) < 1 {
		Fatal("usage: " + CmdUsage["playability"])
	}
	path := rest[0]

	target, ok := matroska.TargetByName(targetName)
	if !ok {
		Fatal(fmt.Sprintf("unknown target %q (known: safari, chrome, firefox, chromecast-gen3, mse-generic, chromium-generic, brave, opera, vivaldi, samsung-internet, edge)", targetName))
	}

	var opts []matroska.Options
	if isRemoteURL(path) {
		opts = append(opts, matroska.Options{FS: remotePort(path)})
	}
	report, err := matroska.Playability(context.Background(), path, target, opts...)
	if err != nil {
		Fatal(err.Error())
	}

	if JsonOutput {
		PrintJSON(report)
		return
	}
	fmt.Printf("Target: %s\n", report.Target)
	for _, tv := range report.Tracks {
		fmt.Printf("  #%d  %-8s  %s", tv.TrackID, tv.Type, tv.Verdict)
		if len(tv.Reasons) > 0 {
			fmt.Printf("  (%s)", strings.Join(tv.Reasons, "; "))
		}
		fmt.Println()
	}
	fmt.Printf("Overall: %s\n", report.OverallVerdict)
	if report.RemuxContainer != "" {
		fmt.Printf("Suggested remux container: %s\n", report.RemuxContainer)
	}
}

// CmdLadder implements `mkvgo ladder`: a recommended ABR ladder derived from
// the file's video track metadata. Guidance, not a guarantee - mkvgo never
// transcodes.
func CmdLadder(args []string) {
	for _, a := range args {
		rejectFlagArg(a)
	}
	if len(args) < 1 {
		Fatal("usage: " + CmdUsage["ladder"])
	}
	path := args[0]

	var opts []matroska.Options
	if isRemoteURL(path) {
		opts = append(opts, matroska.Options{FS: remotePort(path)})
	}
	rungs, err := matroska.RecommendLadderFor(context.Background(), path, opts...)
	if err != nil {
		Fatal(err.Error())
	}

	if JsonOutput {
		PrintJSON(rungs)
		return
	}
	for _, r := range rungs {
		fmt.Printf("  %-6s  %5dx%-5d  %6d kb/s\n", r.Label, r.Width, r.Height, r.BitrateKbps)
	}
}
