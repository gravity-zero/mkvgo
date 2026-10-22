package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mp4"
)

// CmdHash stores each track's content SHA-256 as a CONTENT_SHA256 tag, making
// the file self-verifying (`mkvgo verify`). Without -o the tags are written
// in place.
func CmdHash(args []string) {
	var outPath string
	var rest []string
	fl := newFlags("hash")
	fl.String(&outPath, "-o")
	rest = fl.Parse(args)
	if len(rest) < 1 {
		Fatal("usage: " + CmdUsage["hash"])
	}
	source := rest[0]
	// The content routes, as in diagnose: an MP4 named .mkv gets the MP4 answer, a TS named .mp4 the MPEG-TS refusal.
	if isMP4Content(source) {
		Fatal("mkvgo hashes an MP4 when it writes one, not in place: remux it hashed with mkvgo to-mp4 --hash <in.mkv> <out.mp4>; an edit, split, join or mux of a hashed MP4 stays hashed")
	}
	GuardOverwrite(outPath) // "" (in-place) passes through

	err := matroska.WriteContentHashes(context.Background(), source, outPath,
		matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	if outPath == "" {
		outPath = source
	}
	fmt.Printf("content hashes stored → %s (check them anytime with `mkvgo verify`)\n", outPath)
}

// CmdVerify recomputes the per-track content hashes and compares them with the
// stored hashes (MKV: CONTENT_SHA256 tags; MP4: the freeform atoms written by
// `to-mp4 --hash`). Exits 1 on any mismatch (bit rot, truncation, transfer
// corruption) - scriptable like validate.
func CmdVerify(args []string) {
	newFlags("CmdVerify").Parse(args)
	if len(args) < 1 {
		Fatal("usage: " + CmdUsage["verify"])
	}
	path := args[0]

	report := func(n int, print func()) {
		if JsonOutput || n > 0 {
			print()
		} else {
			fmt.Printf("%s: content OK\n", path)
		}
		if n > 0 {
			osExit(1)
		}
	}

	if isMP4Path(path) {
		mismatches, err := mp4.VerifyContentHashes(context.Background(), path,
			mp4.Options{Progress: NewProgressBar()})
		ClearProgress()
		if err != nil {
			FatalErr(err)
		}
		report(len(mismatches), func() {
			if JsonOutput {
				PrintJSON(mismatches)
				return
			}
			for _, m := range mismatches {
				fmt.Println(m)
			}
		})
		return
	}

	mismatches, err := matroska.VerifyContentHashes(context.Background(), path,
		matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	report(len(mismatches), func() {
		if JsonOutput {
			PrintJSON(mismatches)
			return
		}
		for _, m := range mismatches {
			fmt.Println(m)
		}
	})
}
