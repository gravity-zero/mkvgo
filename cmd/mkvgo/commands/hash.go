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
	for i := 0; i < len(args); i++ {
		if args[i] == "-o" {
			i++
			if i < len(args) {
				outPath = args[i]
			}
			continue
		}
		rejectFlagArg(args[i])
		rest = append(rest, args[i])
	}
	if len(rest) < 1 {
		Fatal("usage: " + CmdUsage["hash"])
	}
	source := rest[0]
	if isMP4Path(source) {
		Fatal("mkvgo does not rewrite MP4 metadata in place; produce a hashed MP4 at remux time: mkvgo to-mp4 --hash <in.mkv> <out.mp4>")
	}
	GuardOverwrite(outPath) // "" (in-place) passes through

	err := matroska.WriteContentHashes(context.Background(), source, outPath,
		matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
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
	for _, a := range args {
		rejectFlagArg(a)
	}
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
			Fatal(err.Error())
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
		Fatal(err.Error())
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
