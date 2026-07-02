package commands

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"

	"github.com/gravity-zero/mkvgo/mp4"
)

// hlsFlags holds the flags to-hls and hls-segment share; they must match
// between the two for byte-identical output.
type hlsFlags struct {
	outDir  string
	segMs   int64
	encrypt *mp4.HLSEncryption
	prefix  string
	rest    []string
}

func parseHLSFlags(args []string) hlsFlags {
	var f hlsFlags
	var keyHex, keyURI string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o":
			i++
			if i < len(args) {
				f.outDir = args[i]
			}
		case "-segment", "--segment":
			// Segment length in seconds (HLS convention, like ffmpeg -hls_time).
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
		case "--url-prefix":
			i++
			if i < len(args) {
				f.prefix = args[i]
			}
		default:
			rejectFlagArg(args[i])
			f.rest = append(f.rest, args[i])
		}
	}
	if keyHex != "" || keyURI != "" {
		key, err := hex.DecodeString(keyHex)
		if err != nil {
			Fatal("invalid --aes-key (expected 32 hex chars): " + err.Error())
		}
		f.encrypt = &mp4.HLSEncryption{Key: key, KeyURI: keyURI}
	}
	return f
}

func (f hlsFlags) options(src string) mp4.Options {
	o := mp4.Options{
		FS:        sourceFS(src),
		SegmentMs: f.segMs,
		Encrypt:   f.encrypt,
	}
	if f.prefix != "" {
		prefix := f.prefix
		o.RewriteURL = func(name string) string { return prefix + name }
	}
	return o
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
// a player requests (seg00042.m4s, sub1.m3u8, sub1.vtt) — so a server answers
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
		// sub1.vtt, master.m3u8, … — the declarative entry point.
		var rerr error
		data, _, rerr = plan.Resource(context.Background(), what)
		if rerr != nil {
			Fatal(rerr.Error())
		}
	}

	if outPath == "" {
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
