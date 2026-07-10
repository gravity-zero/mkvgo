package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
)

// CmdFingerprint prints a container-independent content identity for path: a
// Presentation hash over every track's payload content plus one SHA-256
// digest per track (decode order) - unaffected by container metadata,
// track order, or container format (Matroska/WebM/MP4/MOV), so two re-muxes
// of the same content fingerprint identically. Unlike analyze this is a FULL
// read: every track's frame payload is hashed. MP4/MOV sources are hashed by
// remuxing to a temporary Matroska file first - see matroska.Fingerprint.
func CmdFingerprint(args []string) {
	var rest []string
	for _, a := range args {
		rejectFlagArg(a)
		rest = append(rest, a)
	}
	if len(rest) < 1 {
		Fatal("usage: " + CmdUsage["fingerprint"])
	}
	path := rest[0]

	var opts []matroska.Options
	if isRemoteURL(path) {
		opts = append(opts, matroska.Options{FS: remotePort(path)})
	}

	fp, err := matroska.Fingerprint(context.Background(), path, opts...)
	if err != nil {
		Fatal(err.Error())
	}

	if JsonOutput {
		PrintJSON(fp)
		return
	}
	fmt.Println("Presentation:", fp.Presentation)
	for _, t := range fp.Tracks {
		fmt.Printf("  track %d (%s, %s): %s\n", t.TrackID, t.Type, t.Codec, t.SHA256)
	}
}
