package commands

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"

	"github.com/gravity-zero/mkvgo/mp4"
)

// CmdWatermarkSegment serves one resource of a forensic A/B session-watermarking
// presentation: two GOP-aligned encodes of one title (variant A and B) served as
// one HLS stream whose per-segment bytes come from A or B by a per-viewer bit.
// The manifest (master/playlist/init) is shared; a media segment N is drawn from
// A by default, from B with --variant B, or routed by bit N of --pattern (hex,
// LSB-first). The caller owns the code assignment - which session gets which
// pattern, and collusion-resistant codes.
//
//	mkvgo watermark-segment <a.mkv> <b.mkv> <master|playlist|init|N> \
//	    [--variant A|B] [--pattern <hex>] [-o out] [-segment 6]
func CmdWatermarkSegment(args []string) {
	var outPath, variant, patternHex string
	var segMs int64
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
		default:
			rejectFlagArg(args[i])
			rest = append(rest, args[i])
		}
	}
	if len(rest) < 3 {
		Fatal("usage: " + CmdUsage["watermark-segment"])
	}
	srcA, srcB, what := rest[0], rest[1], rest[2]

	opts := mp4.Options{FS: sourceFS(srcA), SegmentMs: segMs}
	plan, err := mp4.PlanWatermark(context.Background(), srcA, srcB, opts)
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

// watermarkVariant decides whether segment n is drawn from variant B, from
// either an explicit --variant A|B or bit n of a --pattern hex string.
func watermarkVariant(variant, patternHex string, n int) (bool, error) {
	if patternHex != "" {
		pat, err := hex.DecodeString(patternHex)
		if err != nil {
			return false, fmt.Errorf("invalid --pattern (hex): %v", err)
		}
		byteIdx := n / 8
		return byteIdx < len(pat) && pat[byteIdx]&(1<<(uint(n)%8)) != 0, nil
	}
	switch variant {
	case "", "A", "a":
		return false, nil
	case "B", "b":
		return true, nil
	}
	return false, fmt.Errorf("--variant must be A or B, got %q", variant)
}
