package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/gravity-zero/mkvgo/mp4"
)

// CmdConcatHLS packages several sources - played as ONE continuous HLS
// session (e.g. consecutive episodes), no player reload - into an output
// directory (see mp4.RemuxConcatToHLS).
func CmdConcatHLS(args []string) {
	f := parseHLSFlags(args)
	if len(f.rest) < 2 || f.outDir == "" {
		Fatal("usage: " + CmdUsage["concat-hls"])
	}
	opts := f.options(f.rest[0])
	opts.Progress = NewProgressBar()
	opts.OnDrop = func(d mp4.DroppedTrack) {
		fmt.Printf("dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
	}
	err := mp4.RemuxConcatToHLS(context.Background(), f.rest, f.outDir, opts)
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("concatenated HLS session written → %s (play master.m3u8; %d parts, one continuous timeline)\n", f.outDir, len(f.rest))
}

// CmdConcatSegment serves one resource of a concatenated HLS session on
// demand (the on-demand counterpart of concat-hls): nothing is pre-generated.
// The first positional is the resource name ("master.m3u8" or "p{k}/<name>"),
// the rest are the sources in playback order.
func CmdConcatSegment(args []string) {
	f := parseHLSFlags(args)
	outPath, rest := f.outDir, f.rest
	if len(rest) < 3 { // resource + at least two sources
		Fatal("usage: " + CmdUsage["concat-segment"])
	}
	what, sources := rest[0], rest[1:]

	opts := f.options(sources[0])
	opts.OnDrop = func(d mp4.DroppedTrack) {
		fmt.Fprintf(os.Stderr, "dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
	}
	plan, err := mp4.PlanConcat(context.Background(), sources, opts)
	if err != nil {
		Fatal(err.Error())
	}
	data, _, rerr := plan.Resource(context.Background(), what)
	if rerr != nil {
		Fatal(rerr.Error())
	}

	if outPath == "" || outPath == "-" {
		if _, err := os.Stdout.Write(data); err != nil {
			Fatal(err.Error())
		}
		return
	}
	GuardOverwrite(outPath)
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		Fatal(err.Error())
	}
	fmt.Fprintf(os.Stderr, "%s → %s (%d bytes, %d parts)\n", what, outPath, len(data), plan.NumParts())
}
