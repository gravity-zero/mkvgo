package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/ops"
)

// CmdToWebM remuxes an MKV/WebM file to a WebM file. It copies the media
// verbatim, rejects sources whose codecs fall outside the WebM subset
// (VP8/VP9/AV1, Vorbis/Opus, WebVTT), and drops non-WebM elements
// (chapters/attachments/tags).
func CmdToWebM(args []string) {
	if len(args) < 2 {
		Fatal("usage: mkvgo to-webm <input.mkv> <output.webm>")
	}
	src, dst := args[0], args[1]

	err := ops.RemuxToWebM(context.Background(), src, dst, mkv.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("remuxed %s → %s\n", src, dst)
}
