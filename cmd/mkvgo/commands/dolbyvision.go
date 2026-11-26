package commands

import (
	"fmt"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mp4"
)

// dolbyVisionFlag binds --dolby-vision: 8.1 (profile 7 to profile 8.1) or base (the base layer alone).
func dolbyVisionFlag(fl *cliFlags, conv *matroska.DolbyVisionConversion) {
	fl.Func(func(v string) {
		switch v {
		case "8.1":
			*conv = matroska.DolbyVisionToProfile81
		case "base":
			*conv = matroska.DolbyVisionToBaseLayer
		case "keep":
			*conv = matroska.DolbyVisionKeep
		default:
			FatalUsage(fmt.Sprintf("invalid --dolby-vision %q (8.1: a profile 7 track becomes profile 8.1; base: the base layer alone, no Dolby Vision; keep: as stored)", v))
		}
	}, "--dolby-vision")
}

// CmdDolbyVision converts the Dolby Vision layers of a file into a new file of the same container family.
func CmdDolbyVision(args []string) {
	var conv matroska.DolbyVisionConversion
	fl := newFlags("dolby-vision")
	dolbyVisionFlag(fl, &conv)
	rest := fl.Parse(args)
	if len(rest) < 2 || conv == matroska.DolbyVisionKeep {
		FatalUsage("usage: " + CmdUsage["dolby-vision"])
	}
	src, dst := rest[0], rest[1]
	GuardOverwrite(dst)
	guardDistinct(dst, src)
	var err error
	if isMP4Content(src) {
		err = mp4.RemuxFromMP4(cliCtx, src, dst, mp4.Options{FS: sourceFS(src), Progress: NewProgressBar(), Strict: Strict, OnDrop: dropLine, DolbyVision: conv})
	} else {
		err = matroska.ConvertDolbyVision(cliCtx, src, dst, conv, writeOptions())
	}
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	fmt.Printf("converted %s → %s\n", src, dst)
}
