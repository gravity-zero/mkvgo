package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mp4"
)

// CmdKeyframesArgs runs keyframes from its arguments: -walk lists every keyframe of a Matroska that has Cues.
func CmdKeyframesArgs(args []string) {
	var walk bool
	fl := newFlags("keyframes")
	fl.Bool(&walk, "-walk", "--walk")
	source, _ := fl.ParseSource(args, CmdUsage["keyframes"])
	keyframes(source, walk)
}

// CmdKeyframes prints a file's video keyframe timestamps (ms). MKV/WebM reads the
// Cues seek index head-only; MP4 builds the sample table (opt-in). A Cues-less
// Matroska builds the complete keyframe index from a sequential structural pass.
func CmdKeyframes(path string) { keyframes(path, false) }

// keyframes is CmdKeyframes; walk lists a Matroska's keyframes from the structural pass even when it has Cues,
// which may index only some of them. Listed from the Cues, the text form says so on stderr.
func keyframes(path string, walk bool) {
	isMP4, fs := sniffSource(path) // remote: head-only over HTTP Range requests / s3 SigV4
	var ks []int64
	fromCues := false
	if isMP4 {
		c, _, err := mp4.OpenMeta(cliCtx, path, mp4.Options{Keyframes: true, FS: fs})
		if err != nil {
			FatalErr(err)
		}
		ks = c.Keyframes
	} else {
		// Head-only first: a file with Cues lists them. A Cues-less one, or -walk, takes the complete index from the
		// structural pass (the "no external fallback" path). A nil fs is the OS.
		if !walk {
			c, err := matroska.OpenMetaWithFS(cliCtx, path, fs)
			if err != nil {
				FatalErr(err)
			}
			ks, fromCues = c.Keyframes, len(c.Keyframes) > 0
		}
		if !fromCues {
			opt := matroska.WithKeyframeIndex()
			if walk {
				opt = matroska.WithKeyframeWalk()
			}
			c, err := matroska.OpenMetaWithFS(cliCtx, path, fs, opt)
			if err != nil {
				FatalErr(err)
			}
			ks = c.Keyframes
		}
	}

	if JsonOutput {
		if ks == nil {
			ks = []int64{}
		}
		PrintJSON(ks)
		return
	}
	if len(ks) == 0 {
		fmt.Println("No keyframe index available (no Cues index / no video track).")
		return
	}
	for _, ms := range ks {
		fmt.Printf("%s  %d\n", FmtMs(ms), ms)
	}
	if fromCues {
		fmt.Fprintf(os.Stderr, "listed from the Cues index (%d entries): a keyframe between two cues is not listed; -walk lists every one (reads the whole file)\n", len(ks))
	}
}

// CmdToVTT converts an external subtitle sidecar (.srt/.ass/.ssa/.vtt) to WebVTT.
func CmdToVTT(args []string) {
	var outPath string
	var rest []string
	fl := newFlags("to-vtt")
	fl.String(&outPath, "-o")
	rest = fl.Parse(args)
	if len(rest) < 1 || outPath == "" {
		FatalUsage("usage: " + CmdUsage["to-vtt"])
	}
	src := rest[0]
	guardOutput(outPath, src)

	if err := writeFileFrom(outPath, func(out io.Writer) error {
		return matroska.SubtitleFileToWebVTT(src, out)
	}); err != nil {
		FatalErr(err)
	}
	fmt.Printf("converted %s → %s\n", src, outPath)
}

// CmdExtractFrame extracts the video keyframe nearest a timestamp as a
// decoder-ready file (Annex-B or IVF) - the mkvgo half of a thumbnail
// pipeline; feed it to any decoder for a thumbnail.
func CmdExtractFrame(args []string) {
	var outPath string
	var rest []string
	fl := newFlags("extract-frame")
	fl.String(&outPath, "-o")
	rest = fl.Parse(args)
	if len(rest) < 2 || outPath == "" {
		FatalUsage("usage: " + CmdUsage["extract-frame"])
	}
	atMs, err := ParseTimePoint(rest[1])
	if err != nil {
		FatalErr(err)
	}
	ks, err := matroska.ExtractKeyframeSample(cliCtx, rest[0], atMs)
	if err != nil {
		FatalErr(err)
	}
	guardOutput(outPath, rest[0])
	if err := os.WriteFile(outPath, ks.Data, 0o644); err != nil {
		FatalErr(err)
	}
	fmt.Fprintf(os.Stderr, "keyframe @ %s (%s) → %s (%d bytes, decoder-ready)\n",
		FmtMs(ks.PtsMs), ks.Codec, outPath, len(ks.Data))
}

// CmdExtractFrames writes a keyframe-only Matroska stream - the batch form of
// extract-frame, for trickplay strips: one keyframe every -every, or the
// keyframe at or before each -at time, read in one pass over the Cues. The
// stream goes to -o (- for stdout, e.g. piped into a decoder); the extraction
// stats go to stderr, as JSON with -json.
func CmdExtractFrames(args []string) {
	var outPath string
	var sel matroska.KeyframeSelect
	asJSON := false
	var rest []string
	fl := newFlags("extract-frames")
	fl.String(&outPath, "-o")
	timeArg := func(dst *int64, flag string) {
		fl.Func(func(v string) {
			ms, err := ParseTimePoint(v)
			if err != nil {
				FatalUsage(fmt.Sprintf("%s: %v", flag, err))
			}
			*dst = ms
		}, flag)
		fl.Missing(flag, "usage: "+CmdUsage["extract-frames"])
	}
	timeArg(&sel.EveryMs, "-every")
	timeArg(&sel.StartMs, "-start")
	timeArg(&sel.EndMs, "-end")
	fl.Func(func(v string) {
		for _, s := range strings.Split(v, ",") {
			ms, err := ParseTimePoint(s)
			if err != nil {
				FatalUsage(fmt.Sprintf("-at: %v", err))
			}
			sel.TargetsMs = append(sel.TargetsMs, ms)
		}
	}, "-at")
	fl.Missing("-at", "usage: "+CmdUsage["extract-frames"])
	fl.Bool(&asJSON, "-json", "--json")
	rest = fl.Parse(args)
	if len(rest) != 1 || outPath == "" || (sel.EveryMs <= 0) == (len(sel.TargetsMs) == 0) {
		FatalUsage("usage: " + CmdUsage["extract-frames"])
	}
	src := rest[0]
	var opts []mkv.Options
	if isRemoteURL(src) {
		opts = append(opts, mkv.Options{FS: remotePort(src)})
	}
	var st matroska.KeyframeStats
	write := func(out io.Writer) error {
		var err error
		st, err = matroska.WriteKeyframeStream(cliCtx, src, sel, out, opts...)
		return err
	}
	var err error
	if outPath == "-" {
		err = write(os.Stdout)
	} else {
		guardOutput(outPath, src)
		err = writeFileFrom(outPath, write)
	}
	if err != nil {
		FatalErr(err)
	}
	if asJSON || JsonOutput {
		_ = json.NewEncoder(os.Stderr).Encode(st)
		return
	}
	fmt.Fprintf(os.Stderr, "%d keyframes (%d skipped, %d video cues, median cue gap %s) → %s, %d bytes read in %d reads\n",
		st.Samples, st.Skipped, st.Cues, FmtMs(st.MedianCueGapMs), outPath, st.BytesRead+st.MetaBytesRead, st.Reads)
}
