package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/gravity-zero/mkvgo/mp4"
)

// CmdToMP4 remuxes an MKV/WebM file to MP4. Flags: --faststart (moov before
// mdat), --skip-unsupported (drop tracks whose codec MP4 cannot carry),
// --flatten-subs (carry ASS/SSA as plain tx3g, losing styling), --webvtt-native
// (carry WebVTT as lossless wvtt instead of tx3g; not read by ffmpeg).
func CmdToMP4(args []string) {
	var faststart, skip, flatten, wvtt, mp3delay, hashes bool
	var rest []string
	for _, a := range args {
		switch a {
		case "--faststart", "-faststart":
			faststart = true
		case "--hash", "-hash":
			hashes = true
		case "--skip-unsupported", "-skip-unsupported":
			skip = true
		case "--flatten-subs", "-flatten-subs":
			flatten = true
		case "--webvtt-native", "-webvtt-native":
			wvtt = true
		case "--mp3-container-delay", "-mp3-container-delay":
			mp3delay = true
		default:
			rejectFlagArg(a)
			rest = append(rest, a)
		}
	}
	if len(rest) < 2 {
		Fatal("usage: " + CmdUsage["to-mp4"])
	}
	src, dst := rest[0], rest[1]
	GuardOverwrite(dst)

	if flatten {
		fmt.Fprintln(os.Stderr, "warning: --flatten-subs converts styled subtitles to plain text (styling, positioning and karaoke are lost)")
	}
	if wvtt {
		fmt.Fprintln(os.Stderr, "warning: --webvtt-native writes WebVTT as wvtt (read by Apple/CMAF but not by ffmpeg)")
	}

	err := mp4.RemuxToMP4(context.Background(), src, dst, mp4.Options{
		Progress:          NewProgressBar(),
		FastStart:         faststart,
		SkipUnsupported:   skip,
		FlattenStyledSubs: flatten,
		NativeWebVTT:      wvtt,
		MP3ContainerDelay: mp3delay,
		ContentHashes:     hashes,
		OnDrop: func(d mp4.DroppedTrack) {
			fmt.Printf("dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
		},
	})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("remuxed %s → %s\n", src, dst)
}

// CmdFromMP4 remuxes an MP4 file to MKV. --mp3-container-delay carries an MP3's
// edit-list delay into Matroska CodecDelay (opt-in; see Options.MP3ContainerDelay).
func CmdFromMP4(args []string) {
	var mp3delay bool
	var rest []string
	for _, a := range args {
		switch a {
		case "--mp3-container-delay", "-mp3-container-delay":
			mp3delay = true
		default:
			rejectFlagArg(a)
			rest = append(rest, a)
		}
	}
	if len(rest) < 2 {
		Fatal("usage: mkvgo from-mp4 [--mp3-container-delay] <input.mp4> <output.mkv>")
	}
	src, dst := rest[0], rest[1]
	GuardOverwrite(dst)

	err := mp4.RemuxFromMP4(context.Background(), src, dst, mp4.Options{
		Progress:          NewProgressBar(),
		MP3ContainerDelay: mp3delay,
		OnDrop: func(d mp4.DroppedTrack) {
			fmt.Printf("dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
		},
	})
	ClearProgress()
	if err != nil {
		Fatal(err.Error())
	}
	fmt.Printf("remuxed %s → %s\n", src, dst)
}
