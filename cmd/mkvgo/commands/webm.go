package commands

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/ops"
)

// CmdToWebM remuxes an MKV/WebM file to a WebM file. It copies the media
// verbatim, rejects sources whose codecs fall outside the WebM subset
// (VP8/VP9/AV1, Vorbis/Opus, WebVTT), and drops non-WebM elements
// (chapters/attachments/tags) - warning about the ones actually present.
func CmdToWebM(args []string) {
	for _, a := range args {
		rejectFlagArg(a)
	}
	if len(args) < 2 {
		Fatal("usage: mkvgo to-webm <input.mkv> <output.webm>")
	}
	src, dst := args[0], args[1]
	GuardOverwrite(dst)

	// Warn about the non-WebM elements this remux will drop, so the loss is
	// explicit at run time (not only in the docs).
	if c, err := matroska.OpenMeta(context.Background(), src); err == nil {
		if dropped := mkv.WebMNonSubsetElements(c); len(dropped) > 0 {
			fmt.Fprintf(os.Stderr, "warning: %s are not carried into WebM and will be dropped\n",
				strings.Join(dropped, ", "))
		}
	}

	err := ops.RemuxToWebM(context.Background(), src, dst, mkv.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("remuxed %s → %s\n", src, dst)
}
