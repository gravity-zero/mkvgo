package commands

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/gravity-zero/mkvgo/mp4"
)

// CmdForensicSegment serves one resource of a SINGLE-SOURCE forensic A/B
// session-watermarking presentation: variant A segments are the source's
// ordinary HLS segments, variant B segments have one disposable H.264 frame
// dropped (timing-compensated, so the manifest and durations are shared).
// A media segment N is drawn from A by default, from B with --variant B, or
// routed by bit N of --pattern (hex, LSB-first). --distinct prints, instead
// of bytes, whether segment N carries a watermark bit at all.
//
//	mkvgo forensic-segment <src.mkv|mp4> <master|playlist|init|N> \
//	    [--variant A|B] [--pattern <hex>] [--distinct] [-o out] [-segment 6]
func CmdForensicSegment(args []string) {
	var outPath, variant, patternHex string
	var segMs int64
	var distinct bool
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
		case "--variant":
			i++
			if i < len(args) {
				variant = args[i]
			}
		case "--pattern":
			i++
			if i < len(args) {
				patternHex = args[i]
			}
		case "--distinct":
			distinct = true
		default:
			rejectFlagArg(args[i])
			rest = append(rest, args[i])
		}
	}
	if len(rest) < 2 {
		Fatal("usage: " + CmdUsage["forensic-segment"])
	}
	src, what := rest[0], rest[1]

	opts := mp4.Options{FS: sourceFS(src), SegmentMs: segMs}
	plan, err := mp4.PlanForensic(context.Background(), src, opts)
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
		n, aerr := strconv.Atoi(what)
		if aerr != nil || n < 1 || n > plan.NumSegments() {
			Fatal(fmt.Sprintf("resource must be master|playlist|init|N (1..%d), got %q", plan.NumSegments(), what))
		}
		if distinct {
			d, derr := plan.Distinct(context.Background(), n-1)
			if derr != nil {
				Fatal(derr.Error())
			}
			fmt.Printf("segment %d distinct: %v\n", n, d)
			return
		}
		fromB, serr := watermarkVariant(variant, patternHex, n-1)
		if serr != nil {
			Fatal(serr.Error())
		}
		data, err = plan.Segment(context.Background(), n-1, fromB)
		if err != nil {
			Fatal(err.Error())
		}
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
	fmt.Fprintf(os.Stderr, "%s -> %s (%d bytes, %d segments)\n", what, outPath, len(data), plan.NumSegments())
}
