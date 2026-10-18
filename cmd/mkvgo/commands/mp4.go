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
// (carry WebVTT as lossless wvtt instead of tx3g; not read by every demuxer).
func CmdToMP4(args []string) {
	var faststart, skip, flatten, wvtt, mp3delay, hashes bool
	var rest []string
	fl := newFlags("CmdToMP4")
	fl.Bool(&faststart, "--faststart", "-faststart")
	fl.Bool(&hashes, "--hash", "-hash")
	fl.Bool(&skip, "--skip-unsupported", "-skip-unsupported")
	fl.Bool(&flatten, "--flatten-subs", "-flatten-subs")
	fl.Bool(&wvtt, "--webvtt-native", "-webvtt-native")
	fl.Bool(&mp3delay, "--mp3-container-delay", "-mp3-container-delay")
	rest = fl.Parse(args)
	if len(rest) < 2 {
		Fatal("usage: " + CmdUsage["to-mp4"])
	}
	src, dst := rest[0], rest[1]
	GuardOverwrite(dst)

	if flatten {
		fmt.Fprintln(os.Stderr, "warning: --flatten-subs converts styled subtitles to plain text (styling, positioning and karaoke are lost)")
	}
	if wvtt {
		fmt.Fprintln(os.Stderr, "warning: --webvtt-native writes WebVTT as wvtt (read by Apple/CMAF players, not by every demuxer)")
	}

	guardDistinct(dst, src)
	if isMP4Content(src) {
		if skip || flatten || wvtt || mp3delay || hashes {
			Fatal("the source is already an MP4: to-mp4 rewrites it sample for sample (fast start, free space dropped), so --skip-unsupported, --flatten-subs, --webvtt-native, --mp3-container-delay and --hash do not apply")
		}
		err := mp4.Rewrite(context.Background(), src, dst, mp4Options())
		ClearProgress()
		if err != nil {
			FatalErr(err)
		}
		fmt.Printf("rewritten %s → %s\n", src, dst)
		return
	}
	err := mp4.RemuxToMP4(context.Background(), src, dst, mp4.Options{
		FS:                sourceFS(src),
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
		FatalErr(err)
	}
	fmt.Printf("remuxed %s → %s\n", src, dst)
}

// CmdFromMP4 remuxes an MP4 file to MKV. --skip-unsupported drops the tracks Matroska cannot carry (an encrypted
// track, a sample entry without a Matroska form); --mp3-container-delay carries an MP3's edit-list delay into
// Matroska CodecDelay (opt-in; see Options.MP3ContainerDelay).
func CmdFromMP4(args []string) {
	var mp3delay, skip bool
	var rest []string
	fl := newFlags("CmdFromMP4")
	fl.Bool(&mp3delay, "--mp3-container-delay", "-mp3-container-delay")
	fl.Bool(&skip, "--skip-unsupported", "-skip-unsupported")
	rest = fl.Parse(args)
	if len(rest) < 2 {
		Fatal("usage: " + CmdUsage["from-mp4"])
	}
	src, dst := rest[0], rest[1]
	guardOutput(dst, src)

	err := mp4.RemuxFromMP4(context.Background(), src, dst, mp4.Options{
		FS:                sourceFS(src),
		Progress:          NewProgressBar(),
		SkipUnsupported:   skip,
		MP3ContainerDelay: mp3delay,
		OnDrop: func(d mp4.DroppedTrack) {
			fmt.Printf("dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
		},
	})
	ClearProgress()
	if err != nil {
		FatalErr(err)
	}
	fmt.Printf("remuxed %s → %s\n", src, dst)
}
