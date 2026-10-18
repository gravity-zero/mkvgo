package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mp4"
)

// CmdSetChapters replaces a file's chapters from an OGM simple-format text
// file (CHAPTER01=00:00:00.000 / CHAPTER01NAME=Intro - the standard chapter interchange format).
func CmdSetChapters(args []string) {
	usage := CmdUsage["set-chapters"]
	var outPath string
	var rest []string
	fl := newFlags("set-chapters")
	fl.String(&outPath, "-o")
	rest = fl.Parse(args)
	if len(rest) < 2 || outPath == "" {
		Fatal("usage: " + usage)
	}
	source, chapPath := rest[0], rest[1]
	GuardOverwrite(outPath)

	f, err := os.Open(chapPath)
	if err != nil {
		FatalErr(err)
	}
	chapters, err := matroska.ParseOGMChapters(f)
	f.Close()
	if err != nil {
		Fatal(fmt.Sprintf("%s: %v", chapPath, err))
	}

	guardDistinct(outPath, source, chapPath)
	if isMP4Content(source) {
		guardMP4Output(outPath)
		err = mp4.SetChapters(context.Background(), source, outPath, chapters, mp4Options())
	} else {
		err = matroska.SetChapters(context.Background(), source, outPath, chapters,
			matroska.Options{Progress: NewProgressBar()})
	}
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	fmt.Printf("set %d chapters → %s\n", len(chapters), outPath)
}

// CmdExtractChapters prints a file's chapters in the OGM simple format, to
// stdout or to -o <file> - ready for set-chapters and other chapter-aware tools.
func CmdExtractChapters(args []string) {
	usage := CmdUsage["extract-chapters"]
	var outPath string
	var rest []string
	fl := newFlags("extract-chapters")
	fl.String(&outPath, "-o")
	rest = fl.Parse(args)
	if len(rest) < 1 {
		Fatal("usage: " + usage)
	}
	c, _ := loadContainer(rest[0], false)
	if len(c.Chapters) == 0 {
		Fatal("no chapters in " + rest[0])
	}

	if outPath == "" || outPath == "-" {
		if err := matroska.FormatOGMChapters(os.Stdout, c.Chapters); err != nil {
			FatalErr(err)
		}
		return
	}
	guardOutput(outPath, rest[0])
	f, err := os.Create(outPath)
	if err != nil {
		FatalErr(err)
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
