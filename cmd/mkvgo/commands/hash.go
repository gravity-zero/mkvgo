package commands

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
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
// stored CONTENT_SHA256 tags. Exits 1 on any mismatch (bit rot, truncation,
// transfer corruption) — scriptable like validate.
func CmdVerify(args []string) {
	for _, a := range args {
		rejectFlagArg(a)
	}
	if len(args) < 1 {
		Fatal("usage: " + CmdUsage["verify"])
	}
	mismatches, err := matroska.VerifyContentHashes(context.Background(), args[0],
		matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	if JsonOutput {
		PrintJSON(mismatches)
	} else if len(mismatches) == 0 {
		fmt.Printf("%s: content OK\n", args[0])
	} else {
		for _, m := range mismatches {
			fmt.Println(m)
		}
	}
	if len(mismatches) > 0 {
		osExit(1)
	}
}
