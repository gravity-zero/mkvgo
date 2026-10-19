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
	fl := newFlags("forensic-segment")
	fl.String(&outPath, "-o")
	fl.Func(func(v string) {
		segMs = parseSegmentSeconds(v)
	}, "-segment", "--segment")
	fl.String(&variant, "--variant")
	fl.String(&patternHex, "--pattern")
	fl.Bool(&distinct, "--distinct")
	rest = fl.Parse(args)
	if len(rest) < 2 {
		Fatal("usage: " + CmdUsage["forensic-segment"])
	}
	src, what := rest[0], rest[1]

	opts := mp4.Options{FS: sourceFS(src), SegmentMs: segMs}
	plan, err := mp4.PlanForensic(context.Background(), src, opts)
	if err != nil {
		FatalErr(err)
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
			FatalErr(err)
		}
	}

	if outPath == "" || outPath == "-" {
		if _, err := os.Stdout.Write(data); err != nil {
			FatalErr(err)
		}
		return
	}
	guardOutput(outPath, src)
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		FatalErr(err)
	}
	fmt.Fprintf(os.Stderr, "%s -> %s (%d bytes, %d segments)\n", what, outPath, len(data), plan.NumSegments())
}
