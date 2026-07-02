package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/gravity-zero/mkvgo/matroska"
)

// CmdSetChapters replaces a file's chapters from an OGM simple-format text
// file (CHAPTER01=00:00:00.000 / CHAPTER01NAME=Intro — the format mkvmerge
// and ffmpeg understand).
func CmdSetChapters(args []string) {
	usage := CmdUsage["set-chapters"]
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
	if len(rest) < 2 || outPath == "" {
		Fatal("usage: " + usage)
	}
	source, chapPath := rest[0], rest[1]
	GuardOverwrite(outPath)

	f, err := os.Open(chapPath)
	if err != nil {
		Fatal(err.Error())
	}
	chapters, err := matroska.ParseOGMChapters(f)
	f.Close()
	if err != nil {
		Fatal(fmt.Sprintf("%s: %v", chapPath, err))
	}

	err = matroska.SetChapters(context.Background(), source, outPath, chapters,
		matroska.Options{Progress: NewProgressBar()})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("set %d chapters → %s\n", len(chapters), outPath)
}

// CmdExtractChapters prints a file's chapters in the OGM simple format, to
// stdout or to -o <file> — ready for mkvmerge --chapters or set-chapters.
func CmdExtractChapters(args []string) {
	usage := CmdUsage["extract-chapters"]
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
		Fatal("usage: " + usage)
	}
	c, _ := loadContainer(rest[0], false)
	if len(c.Chapters) == 0 {
		Fatal("no chapters in " + rest[0])
	}

	if outPath == "" || outPath == "-" {
		if err := matroska.FormatOGMChapters(os.Stdout, c.Chapters); err != nil {
			Fatal(err.Error())
		}
		return
	}
	GuardOverwrite(outPath)
	f, err := os.Create(outPath)
	if err != nil {
		Fatal(err.Error())
	}
	werr := matroska.FormatOGMChapters(f, c.Chapters)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		Fatal(werr.Error())
	}
	fmt.Printf("extracted %d chapters → %s\n", len(c.Chapters), outPath)
}
