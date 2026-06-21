package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/mp4"
)

// CmdToMP4 remuxes an MKV/WebM file to MP4. Flags: --faststart (moov before
// mdat), --skip-unsupported (drop tracks whose codec MP4 cannot carry).
func CmdToMP4(args []string) {
	var faststart, skip bool
	var rest []string
	for _, a := range args {
		switch a {
		case "--faststart", "-faststart":
			faststart = true
		case "--skip-unsupported", "-skip-unsupported":
			skip = true
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) < 2 {
		Fatal("usage: mkvgo to-mp4 [--faststart] [--skip-unsupported] <input.mkv> <output.mp4>")
	}
	src, dst := rest[0], rest[1]

	err := mp4.RemuxToMP4(context.Background(), src, dst, mp4.Options{
		Progress:        NewProgressBar(),
		FastStart:       faststart,
		SkipUnsupported: skip,
		OnDrop: func(d mp4.DroppedTrack) {
			fmt.Printf("dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
		},
	})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("remuxed %s → %s\n", src, dst)
}

// CmdFromMP4 remuxes an MP4 file to MKV.
func CmdFromMP4(args []string) {
	if len(args) < 2 {
		Fatal("usage: mkvgo from-mp4 <input.mp4> <output.mkv>")
	}
	src, dst := args[0], args[1]

	err := mp4.RemuxFromMP4(context.Background(), src, dst, mp4.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("remuxed %s → %s\n", src, dst)
}
