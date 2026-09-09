package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
	"github.com/gravity-zero/mkvgo/mp4"
)

func CmdExtractAttachment(args []string) {
	if len(args) < 4 {
		Fatal("usage: mkvgo extract-attachment <file.mkv> <attachmentID> -o <outfile>")
	}
	source := args[0]
	attID, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		Fatal(fmt.Sprintf("invalid attachment ID %q", args[1]))
	}
	var outPath string
	for i := 2; i < len(args); i++ {
		if args[i] == "-o" {
			i++
			if i >= len(args) {
				Fatal("-o needs a value")
			}
			outPath = args[i]
		} else {
			rejectFlagArg(args[i])
		}
	}
	if outPath == "" {
		Fatal("usage: mkvgo extract-attachment <file.mkv> <attachmentID> -o <outfile>")
	}
	GuardOverwrite(outPath)

	err = matroska.ExtractAttachment(context.Background(), source, attID, outPath)
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("extracted attachment #%d → %s\n", attID, outPath)
}

func CmdExtractSubtitle(args []string) {
	usage := CmdUsage["extract-subtitle"]
	if len(args) < 5 {
		Fatal("usage: " + usage)
	}
	source := args[0]
	var outPath, format, indexPath string
	var trackID uint64
	format = "srt"

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			if i >= len(args) {
				Fatal("-o needs a value")
			}
			outPath = args[i]
		case "-index":
			i++
			if i >= len(args) {
				Fatal("-index needs a value")
			}
			indexPath = args[i]
		case "-t":
			i++
			if i >= len(args) {
				Fatal("-t needs a value")
			}
			id, err := strconv.ParseUint(args[i], 10, 64)
			if err != nil {
				Fatal(fmt.Sprintf("invalid track ID %q", args[i]))
			}
			trackID = id
		case "-format":
			i++
			if i >= len(args) {
				Fatal("-format needs a value")
			}
			format = args[i]
		default:
			rejectFlagArg(args[i])
		}
	}
	if outPath == "" || trackID == 0 {
		Fatal("usage: " + usage)
	}
	GuardOverwrite(outPath)

	if indexPath != "" {
		if format != "vtt" && format != "pgs" {
			Fatal("-index applies to -format vtt and -format pgs only")
		}
		if isMP4Path(source) {
			Fatal("-index applies to MKV/WebM only: an MP4 already carries its own sample table")
		}
		n, err := extractFromIndex(source, trackID, format, indexPath, outPath)
		if err != nil {
			Fatal(err.Error())
		}
		if format == "pgs" {
			fmt.Printf("extracted %d PGS cue(s) from subtitle track %d (from %s) → %s\n", n, trackID, indexPath, outPath)
			return
		}
		fmt.Printf("extracted subtitle track %d (vtt, from %s) → %s\n", trackID, indexPath, outPath)
		return
	}

	var err error
	switch format {
	case "vtt":
		err = extractWebVTT(source, trackID, outPath)
	case "pgs":
		if isMP4Path(source) {
			Fatal("PGS bitmap subtitles are a Matroska codec: -format pgs applies to MKV/WebM only")
		}
		var n int
		n, err = writePGSDir(outPath, func(fn func(matroska.PGSCue) error) error {
			return matroska.ForEachSubtitlePGS(context.Background(), source, trackID, fn)
		})
		if err == nil {
			fmt.Printf("extracted %d PGS cue(s) from subtitle track %d → %s\n", n, trackID, outPath)
			return
		}
	case "srt":
		if isMP4Path(source) {
			Fatal("MP4 subtitle extraction supports only -format vtt")
		}
		err = matroska.ExtractSubtitle(context.Background(), source, trackID, outPath)
	case "ass", "ssa":
		if isMP4Path(source) {
			Fatal("MP4 subtitle extraction supports only -format vtt")
		}
		err = matroska.ExtractASS(context.Background(), source, trackID, outPath)
	default:
		Fatal(fmt.Sprintf("unknown format %q (supported: srt, ass, vtt, pgs)", format))
	}
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("extracted subtitle track %d (%s) → %s\n", trackID, format, outPath)
}

// extractWebVTT writes subtitle track trackID of source (MKV/WebM or MP4) to
// outPath as WebVTT.
func extractWebVTT(source string, trackID uint64, outPath string) error {
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	if isMP4Path(source) {
		return mp4.ExtractSubtitleWebVTT(context.Background(), source, trackID, out)
	}
	return matroska.ExtractSubtitleWebVTT(context.Background(), source, trackID, out)
}

// extractFromIndex serves one track from an index file written by
// CmdSubtitleIndex, instead of walking the source. It returns the number of PGS
// cues written, which is 0 (and meaningless) for the text formats.
func extractFromIndex(source string, trackID uint64, format, indexPath, outPath string) (int, error) {
	blob, err := os.ReadFile(indexPath)
	if err != nil {
		return 0, err
	}
	var ix matroska.SubtitleIndex
	if err := ix.UnmarshalBinary(blob); err != nil {
		return 0, err
	}
	if format == "pgs" {
		return writePGSDir(outPath, func(fn func(matroska.PGSCue) error) error {
			return matroska.ForEachSubtitlePGSFrom(context.Background(), source, trackID, &ix, fn)
		})
	}
	out, err := os.Create(outPath)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	return 0, matroska.ExtractSubtitleWebVTTFrom(context.Background(), source, trackID, &ix, out)
}

// pgsCueManifest is one line of the manifest written beside the pictures. The
// CLI writes an on-disk form of PGSCue and stops there: turning bitmaps into
// something a player renders (a sprite sheet, an OCR pass) is a packaging
// decision that belongs to the consumer, not to an extractor.
type pgsCueManifest struct {
	Index   int    `json:"index"`
	StartMs int64  `json:"start_ms"`
	EndMs   int64  `json:"end_ms"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	ScreenW int    `json:"screen_w"`
	ScreenH int    `json:"screen_h"`
	Forced  bool   `json:"forced,omitempty"`
	File    string `json:"file"`
}

// writePGSDir writes one PNG per cue into outDir plus a cues.json manifest. It
// drives the streaming extractor, so a two-hour track costs one picture of
// memory rather than the whole track's worth.
func writePGSDir(outDir string, each func(func(matroska.PGSCue) error) error) (int, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return 0, err
	}
	var manifest []pgsCueManifest
	n := 0
	err := each(func(c matroska.PGSCue) error {
		name := fmt.Sprintf("%05d.png", n)
		f, err := os.Create(filepath.Join(outDir, name))
		if err != nil {
			return err
		}
		// A PGS cue holds at most 256 colours, so truecolour spends four bytes
		// a pixel to say what one can - about a third more on disk for output
		// a consumer will cache. Fall back to the picture as decoded if it
		// somehow does not fit a palette.
		var img image.Image = c.Image
		if p := subtitle.Paletted(c.Image); p != nil {
			img = p
		}
		if err := png.Encode(f, img); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		manifest = append(manifest, pgsCueManifest{
			Index: n, StartMs: c.StartMs, EndMs: c.EndMs, X: c.X, Y: c.Y,
			ScreenW: c.ScreenW, ScreenH: c.ScreenH, Forced: c.Forced, File: name,
		})
		n++
		return nil
	})
	if err != nil {
		return n, err
	}
	blob, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return n, err
	}
	return n, os.WriteFile(filepath.Join(outDir, "cues.json"), append(blob, '\n'), 0o644)
}

// CmdSubtitleIndex builds the subtitle block index of an MKV/WebM and writes it
// out. Matroska's Cues index the video track only, so without this every
// subtitle extraction re-walks the whole file; with it, extraction seeks.
func CmdSubtitleIndex(args []string) {
	usage := CmdUsage["subtitle-index"]
	if len(args) < 3 {
		Fatal("usage: " + usage)
	}
	source := args[0]
	var outPath string
	var trackIDs []uint64
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			if i >= len(args) {
				Fatal("-o needs a value")
			}
			outPath = args[i]
		case "-t":
			i++
			if i >= len(args) {
				Fatal("-t needs a value")
			}
			for _, part := range strings.Split(args[i], ",") {
				id, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64)
				if err != nil {
					Fatal(fmt.Sprintf("invalid track ID %q", part))
				}
				trackIDs = append(trackIDs, id)
			}
		default:
			rejectFlagArg(args[i])
		}
	}
	if outPath == "" {
		Fatal("usage: " + usage)
	}
	GuardOverwrite(outPath)

	ix, err := matroska.BuildSubtitleIndex(context.Background(), source, trackIDs)
	if err != nil {
		Fatal(err.Error())
	}
	blob, err := ix.MarshalBinary()
	if err != nil {
		Fatal(err.Error())
	}
	if err := os.WriteFile(outPath, blob, 0o644); err != nil {
		Fatal(err.Error())
	}
	total := 0
	for _, id := range ix.Tracks() {
		total += ix.Blocks(id)
	}
	fmt.Printf("indexed %d blocks over %d subtitle track(s) → %s (%d bytes)\n",
		total, len(ix.Tracks()), outPath, len(blob))
}
