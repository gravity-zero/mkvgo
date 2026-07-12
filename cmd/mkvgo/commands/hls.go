package commands

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mp4"
)

// hlsFlags holds the flags to-hls and hls-segment share; they must match
// between the two for byte-identical output.
type hlsFlags struct {
	outDir         string
	segMs          int64
	encrypt        *mp4.HLSEncryption
	cenc           *mp4.CENCOptions
	prefix         string
	singleFile     bool
	keepTracks     []uint64
	keepLangs      []string
	subOffset      int64
	chapterMarkers bool
	synthIndex     bool
	audioShift     map[uint64]int64
	rest           []string
}

func parseHLSFlags(args []string) hlsFlags {
	var f hlsFlags
	var keyHex, keyURI string
	var rotateSegs int
	var cencScheme, cencKeyHex, cencKIDHex, cencIVHex, cencKeyURI string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			if i < len(args) {
				f.outDir = args[i]
			}
		case "-segment", "--segment":
			// Segment length in seconds (HLS convention, the HLS convention).
			i++
			if i < len(args) {
				secs, err := strconv.ParseFloat(args[i], 64)
				if err != nil || secs <= 0 {
					Fatal(fmt.Sprintf("invalid -segment duration %q (seconds)", args[i]))
				}
				f.segMs = int64(secs * 1000)
			}
		case "--aes-key":
			i++
			if i < len(args) {
				keyHex = args[i]
			}
		case "--aes-key-uri":
			i++
			if i < len(args) {
				keyURI = args[i]
			}
		case "--aes-rotate-segments":
			i++
			if i < len(args) {
				n, err := strconv.Atoi(args[i])
				if err != nil || n < 1 {
					Fatal(fmt.Sprintf("invalid --aes-rotate-segments %q (positive integer)", args[i]))
				}
				rotateSegs = n
			}
		case "--cenc-scheme":
			i++
			if i < len(args) {
				cencScheme = args[i]
			}
		case "--cenc-key":
			i++
			if i < len(args) {
				cencKeyHex = args[i]
			}
		case "--cenc-kid":
			i++
			if i < len(args) {
				cencKIDHex = args[i]
			}
		case "--cenc-iv":
			i++
			if i < len(args) {
				cencIVHex = args[i]
			}
		case "--cenc-key-uri":
			i++
			if i < len(args) {
				cencKeyURI = args[i]
			}
		case "--url-prefix":
			i++
			if i < len(args) {
				f.prefix = args[i]
			}
		case "--single-file":
			f.singleFile = true
		case "--keep-tracks":
			i++
			if i < len(args) {
				for _, p := range strings.Split(args[i], ",") {
					id, err := strconv.ParseUint(strings.TrimSpace(p), 10, 64)
					if err != nil {
						Fatal(fmt.Sprintf("invalid --keep-tracks id %q (comma-separated track IDs)", p))
					}
					f.keepTracks = append(f.keepTracks, id)
				}
			}
		case "--keep-lang", "--audio-lang":
			i++
			if i < len(args) {
				for _, l := range strings.Split(args[i], ",") {
					if l = strings.TrimSpace(l); l != "" {
						f.keepLangs = append(f.keepLangs, strings.ToLower(l))
					}
				}
			}
		case "--sub-offset":
			i++
			if i < len(args) {
				ms, err := strconv.ParseInt(args[i], 10, 64)
				if err != nil {
					Fatal(fmt.Sprintf("invalid --sub-offset %q (integer milliseconds, negative allowed)", args[i]))
				}
				f.subOffset = ms
			}
		case "--chapter-markers":
			f.chapterMarkers = true
		case "--synthesize-index":
			f.synthIndex = true
		case "--audio-shift":
			if i+1 >= len(args) {
				Fatal("--audio-shift needs a <track>=<ms> value")
			}
			i++
			track, ms, err := parseShift(args[i])
			if err != nil {
				Fatal(strings.ReplaceAll(err.Error(), "--shift", "--audio-shift"))
			}
			if f.audioShift == nil {
				f.audioShift = map[uint64]int64{}
			}
			f.audioShift[track] = ms * 1_000_000 // ms -> ns
		default:
			rejectFlagArg(args[i])
			f.rest = append(f.rest, args[i])
		}
	}
	if keyHex != "" || keyURI != "" {
		// --aes-key and --aes-key-uri take a single value, or a comma-separated
		// list when rotating (--aes-rotate-segments N): the keys cycle every N
		// segments, key i served at URI i, so a captured key expires mid-video.
		if rotateSegs > 0 {
			keys := splitCSV(keyHex)
			uris := splitCSV(keyURI)
			if len(keys) < 2 || len(keys) != len(uris) {
				Fatal("--aes-rotate-segments needs matching comma-separated --aes-key and --aes-key-uri lists of at least 2 entries")
			}
			var ks []mp4.HLSKey
			for j := range keys {
				k, err := hex.DecodeString(keys[j])
				if err != nil {
					Fatal(fmt.Sprintf("invalid --aes-key entry %d (expected 32 hex chars): %s", j+1, err.Error()))
				}
				ks = append(ks, mp4.HLSKey{Key: k, KeyURI: uris[j]})
			}
			f.encrypt = &mp4.HLSEncryption{RotateEverySegments: rotateSegs, Keys: ks}
		} else {
			key, err := hex.DecodeString(keyHex)
			if err != nil {
				Fatal("invalid --aes-key (expected 32 hex chars): " + err.Error())
			}
			f.encrypt = &mp4.HLSEncryption{Key: key, KeyURI: keyURI}
		}
	}
	if cencScheme != "" || cencKeyHex != "" || cencKIDHex != "" || cencIVHex != "" {
		key := mustHex(cencKeyHex, "--cenc-key (expected 32 hex chars)")
		kid := mustHex(cencKIDHex, "--cenc-kid (expected 32 hex chars)")
		iv := mustHex(cencIVHex, "--cenc-iv (expected 16 or 32 hex chars)")
		f.cenc = &mp4.CENCOptions{Scheme: cencScheme, Key: key, KeyID: kid, IV: iv, KeyURI: cencKeyURI}
	}
	return f
}

// splitCSV splits a comma-separated flag value into trimmed, non-empty parts.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// mustHex decodes a hex-encoded flag value, exiting with a clear error on
// malformed input (odd length, non-hex characters) - validated up front
// rather than surfacing as a confusing key/IV-length error from mp4.CENCOptions.
func mustHex(s, what string) []byte {
	if s == "" {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		Fatal("invalid " + what + ": " + err.Error())
	}
	return b
}

func (f hlsFlags) options(src string) mp4.Options {
	keep := f.keepTracks
	if len(keep) == 0 && len(f.keepLangs) > 0 {
		keep = resolveKeepLangs(src, f.keepLangs)
	}
	o := mp4.Options{
		FS:                     sourceFS(src),
		SegmentMs:              f.segMs,
		Encrypt:                f.encrypt,
		CENC:                   f.cenc,
		SingleFile:             f.singleFile,
		KeepTracks:             keep,
		SubtitleOffsetMs:       f.subOffset,
		ChapterMarkers:         f.chapterMarkers,
		SynthesizeIndex:        f.synthIndex,
		AudioPresentationShift: f.audioShift,
	}
	if f.prefix != "" {
		prefix := f.prefix
		o.RewriteURL = func(name string) string { return prefix + name }
	}
	return o
}

// resolveKeepLangs turns --keep-lang codes into a KeepTracks ID list: every
// video track (HLS needs video) plus every audio/subtitle track whose language
// matches. It is CLI sugar over KeepTracks - a library caller that already has
// the track metadata builds the ID list itself. When a language maps to several
// tracks (e.g. VFF + VFQ, both "fre") all are kept; use --keep-tracks for exact
// control.
func resolveKeepLangs(src string, langs []string) []uint64 {
	c, _ := loadContainer(src, false)
	want := make(map[string]bool, len(langs))
	for _, l := range langs {
		want[l] = true
	}
	var ids []uint64
	for _, t := range c.Tracks {
		if t.Type == mkv.VideoTrack || want[strings.ToLower(t.Language)] {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

// CmdToHLS remuxes an MKV/WebM file to a fragmented-MP4 HLS presentation
// (init.mp4 + segments + playlist.m3u8) in an output directory.
func CmdToHLS(args []string) {
	f := parseHLSFlags(args)
	outDir, rest := f.outDir, f.rest
	if len(rest) < 1 || outDir == "" {
		Fatal("usage: " + CmdUsage["to-hls"])
	}
	src := rest[0]

	opts := f.options(src)
	opts.Progress = NewProgressBar()
	opts.OnDrop = func(d mp4.DroppedTrack) {
		fmt.Printf("dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
	}
	err := mp4.RemuxToHLS(context.Background(), src, outDir, opts)
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("HLS written → %s (play master.m3u8; init.mp4 + segments + subtitle renditions)\n", outDir)
}

// CmdHLSSegment serves one resource of an on-demand HLS plan: the master or
// media playlist, the init segment, the n-th media segment (built by reading
// only that segment's window, seeked through the Cues), or any resource name
// a player requests (seg00042.m4s, sub1.m3u8, sub1.vtt) - so a server answers
// HLS requests with no pre-generated files. The source may be a local path or
// an http(s) URL.
func CmdHLSSegment(args []string) {
	f := parseHLSFlags(args)
	outPath, rest := f.outDir, f.rest
	if len(rest) < 2 {
		Fatal("usage: " + CmdUsage["hls-segment"])
	}
	src, what := rest[0], rest[1]

	opts := f.options(src)
	opts.OnDrop = func(d mp4.DroppedTrack) {
		fmt.Fprintf(os.Stderr, "dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
	}
	plan, err := mp4.PlanHLS(context.Background(), src, opts)
	if err != nil {
		Fatal(err.Error())
	}

	var data []byte
	switch what {
	case "master":
		data = plan.MasterPlaylist()
	case "playlist":
		data = plan.MediaPlaylist()
	case "init":
		data = plan.InitSegment()
	default:
		if n, aerr := strconv.Atoi(what); aerr == nil {
			if n < 1 || n > plan.NumSegments() {
				Fatal(fmt.Sprintf("segment %d out of range (1..%d)", n, plan.NumSegments()))
			}
			var serr error
			data, serr = plan.Segment(context.Background(), n-1)
			if serr != nil {
				Fatal(serr.Error())
			}
			break
		}
		// Any resource name a player would request: seg00042.m4s, sub1.m3u8,
		// sub1.vtt, master.m3u8, … - the declarative entry point.
		var rerr error
		data, _, rerr = plan.Resource(context.Background(), what)
		if rerr != nil {
			Fatal(rerr.Error())
		}
	}

	if outPath == "" || outPath == "-" {
		if _, err := os.Stdout.Write(data); err != nil {
			Fatal(err.Error())
		}
		return
	}
	GuardOverwrite(outPath)
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		Fatal(err.Error())
	}
	fmt.Fprintf(os.Stderr, "%s → %s (%d bytes, %d segments total)\n", what, outPath, len(data), plan.NumSegments())
}

// CmdToABR packages several pre-encoded quality variants of the same content
// into one multi-variant HLS presentation (no transcoding): the first source
// is the reference (audio + subtitles), the others contribute their video.
func CmdToABR(args []string) {
	f := parseHLSFlags(args)
	if len(f.rest) < 2 || f.outDir == "" {
		Fatal("usage: " + CmdUsage["to-abr"])
	}
	opts := f.options(f.rest[0])
	opts.Progress = NewProgressBar()
	opts.OnDrop = func(d mp4.DroppedTrack) {
		fmt.Printf("dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
	}
	err := mp4.RemuxToABR(context.Background(), f.rest, f.outDir, opts)
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("ABR presentation written → %s (play master.m3u8; %d variants)\n", f.outDir, len(f.rest))
}

// CmdABRSegment serves one resource of a multi-variant ABR presentation on
// demand (the on-demand counterpart of to-abr): nothing is pre-generated, each
// resource is built when asked. The first positional is the resource name
// ("master.m3u8" or "v{k}/<name>"), the rest are the quality variants best
// first.
func CmdABRSegment(args []string) {
	f := parseHLSFlags(args)
	outPath, rest := f.outDir, f.rest
	if len(rest) < 3 { // resource + at least two variants
		Fatal("usage: " + CmdUsage["abr-segment"])
	}
	what, sources := rest[0], rest[1:]

	opts := f.options(sources[0])
	opts.OnDrop = func(d mp4.DroppedTrack) {
		fmt.Fprintf(os.Stderr, "dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
	}
	plan, err := mp4.PlanABR(context.Background(), sources, opts)
	if err != nil {
		Fatal(err.Error())
	}
	data, _, rerr := plan.Resource(context.Background(), what)
	if rerr != nil {
		Fatal(rerr.Error())
	}

	if outPath == "" || outPath == "-" {
		if _, err := os.Stdout.Write(data); err != nil {
			Fatal(err.Error())
		}
		return
	}
	GuardOverwrite(outPath)
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		Fatal(err.Error())
	}
	fmt.Fprintf(os.Stderr, "%s → %s (%d bytes, %d variants)\n", what, outPath, len(data), plan.NumVariants())
}
