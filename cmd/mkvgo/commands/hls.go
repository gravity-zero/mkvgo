package commands

import (
	"context"
	"crypto/rand"
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
	outDir          string
	segMs           int64
	encrypt         *mp4.HLSEncryption
	cenc            *mp4.CENCOptions
	prefix          string
	singleFile      bool
	keepTracks      []uint64
	keepLangs       []string
	subOffset       int64
	chapterMarkers  bool
	noIFrames       bool
	damageBytes     int64
	skipUnsupported bool
	sourceAccess    mp4.SourceAccess
	synthIndex      bool
	audioShift      map[uint64]int64
	rest            []string
}

func parseHLSFlags(args []string) hlsFlags {
	var f hlsFlags
	var keyHex, keyURI string
	var rotateSegs int
	var cencScheme, cencKeyHex, cencKIDHex, cencIVHex, cencKeyURI string
	var keyFile, cencKeyFile string
	var cencInline bool
	fl := newFlags("hls")
	fl.String(&f.outDir, "-o")
	fl.Func(func(v string) { // segment length in seconds, the HLS convention
		f.segMs = parseSegmentSeconds(v)
	}, "-segment", "--segment")
	fl.String(&keyHex, "--aes-key")
	fl.String(&keyFile, "--aes-key-file")
	fl.String(&cencKeyFile, "--cenc-key-file")
	fl.Bool(&cencInline, "--cenc-inline-key")
	fl.String(&keyURI, "--aes-key-uri")
	fl.Func(func(v string) {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			Fatal(fmt.Sprintf("invalid --aes-rotate-segments %q (positive integer)", v))
		}
		rotateSegs = n
	}, "--aes-rotate-segments")
	fl.String(&cencScheme, "--cenc-scheme")
	fl.String(&cencKeyHex, "--cenc-key")
	fl.String(&cencKIDHex, "--cenc-kid")
	fl.String(&cencIVHex, "--cenc-iv")
	fl.String(&cencKeyURI, "--cenc-key-uri")
	fl.String(&f.prefix, "--url-prefix")
	fl.Bool(&f.singleFile, "--single-file")
	fl.Func(func(v string) {
		for _, p := range strings.Split(v, ",") {
			id, err := strconv.ParseUint(strings.TrimSpace(p), 10, 64)
			if err != nil {
				Fatal(fmt.Sprintf("invalid --keep-tracks id %q (comma-separated track IDs)", p))
			}
			f.keepTracks = append(f.keepTracks, id)
		}
	}, "--keep-tracks")
	fl.Func(func(v string) {
		for _, l := range strings.Split(v, ",") {
			if l = strings.TrimSpace(l); l != "" {
				f.keepLangs = append(f.keepLangs, strings.ToLower(l))
			}
		}
	}, "--keep-lang", "--audio-lang")
	fl.Func(func(v string) {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			Fatal(fmt.Sprintf("invalid --sub-offset %q (integer milliseconds, negative allowed)", v))
		}
		f.subOffset = ms
	}, "--sub-offset")
	fl.Bool(&f.chapterMarkers, "--chapter-markers")
	fl.Bool(&f.skipUnsupported, "--skip-unsupported", "-skip-unsupported")
	fl.Bool(&f.noIFrames, "--no-iframes")
	fl.Func(damageBytesFlag(&f.damageBytes), "--damage-bytes")
	fl.Func(func(v string) {
		switch v {
		case "auto":
			f.sourceAccess = mp4.SourceAuto
		case "local":
			f.sourceAccess = mp4.SourceLocal
		case "network":
			f.sourceAccess = mp4.SourceNetwork
		default:
			Fatal("--source-access takes auto, local or network, not " + v)
		}
	}, "--source-access")
	fl.Missing("--source-access", "--source-access needs auto, local or network")
	fl.Bool(&f.synthIndex, "--synthesize-index")
	fl.Func(func(v string) {
		track, ms, err := parseShift(v)
		if err != nil {
			Fatal(strings.ReplaceAll(err.Error(), "--shift", "--audio-shift"))
		}
		if f.audioShift == nil {
			f.audioShift = map[uint64]int64{}
		}
		f.audioShift[track] = ms * 1_000_000 // ms -> ns
	}, "--audio-shift")
	fl.Missing("--audio-shift", "--audio-shift needs a <track>=<ms> value")
	f.rest = fl.Parse(args)
	keyHex = keyValue(keyHex, keyFile, "MKVGO_AES_KEY", "--aes-key", keyURI != "")
	cencKeyHex = keyValue(cencKeyHex, cencKeyFile, "MKVGO_CENC_KEY", "--cenc-key", cencScheme != "" || cencKIDHex != "" || cencIVHex != "" || cencKeyURI != "")
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
		if cencKeyURI == "" && !cencInline {
			Fatal("--cenc-key-uri is required: without it the playlist would carry the content key in clear (a data: URI). Pass --cenc-key-uri with the URI of your key or license server, or --cenc-inline-key to embed the key on purpose (testing only)")
		}
		f.cenc = &mp4.CENCOptions{Scheme: cencScheme, Key: key, KeyID: kid, IV: iv, KeyURI: cencKeyURI, InlineKey: cencInline}
	}
	return f
}

// keyValue is a key's hex from its argv flag, else its file (whitespace
// ignored), else env when the encryption it belongs to is asked for.
func keyValue(argv, file, env, flag string, asked bool) string {
	if argv != "" && file != "" {
		Fatal(flag + " and " + flag + "-file both given: pass the key once")
	}
	switch {
	case argv != "":
		return argv
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			Fatal("cannot read " + flag + "-file: " + err.Error())
		}
		return strings.Join(strings.Fields(string(b)), "")
	case asked:
		return os.Getenv(env)
	}
	return ""
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
// cencRandRead fills a generated CENC IV.
var cencRandRead = rand.Read

// randomCENCIV gives a CENC presentation packaged without --cenc-iv a random IV, printed so it can be packaged again alike.
func (f *hlsFlags) randomCENCIV() {
	if f.cenc == nil || f.cenc.IV != nil {
		return
	}
	iv := make([]byte, 16)
	if f.cenc.Scheme == "cenc" {
		iv = iv[:8]
	}
	if _, err := cencRandRead(iv); err != nil {
		Fatal("cannot generate a CENC IV: " + err.Error() + "; pass --cenc-iv")
	}
	f.cenc.IV = iv
	fmt.Fprintf(os.Stderr, "--cenc-iv not given: generated %x. Pass --cenc-iv %x to package or serve this presentation again with the same bytes (hls-segment, abr-segment and concat-segment need it)\n", iv, iv)
}

// needCENCIV refuses an on-demand resource without --cenc-iv: a fresh IV per call would not match the rest of the presentation.
func (f hlsFlags) needCENCIV() {
	if f.cenc != nil && f.cenc.IV == nil {
		Fatal("--cenc-iv is required here: each call packages one resource, which must use the IV of the rest of the presentation. Pass the --cenc-iv the presentation was packaged with (to-hls prints the one it generates)")
	}
}

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
		NoIFrames:              f.noIFrames,
		DamageBytes:            f.damageBytes,
		SkipUnsupported:        f.skipUnsupported,
		SynthesizeIndex:        f.synthIndex,
		AudioPresentationShift: f.audioShift,
		SourceAccess:           f.sourceAccess,
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
	f.randomCENCIV()
	src := rest[0]

	opts := f.options(src)
	opts.Progress = NewProgressBar()
	opts.OnDrop = func(d mp4.DroppedTrack) {
		fmt.Printf("dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
	}
	err := mp4.RemuxToHLS(context.Background(), src, outDir, opts)
	ClearProgress()
	if err != nil {
		FatalErr(err)
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
	f.needCENCIV()
	src, what := rest[0], rest[1]

	opts := f.options(src)
	opts.OnDrop = func(d mp4.DroppedTrack) {
		fmt.Fprintf(os.Stderr, "dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
	}
	plan, err := mp4.PlanHLS(context.Background(), src, opts)
	if err != nil {
		FatalErr(err)
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
			FatalErr(err)
		}
		return
	}
	guardOutput(outPath, src)
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		FatalErr(err)
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
	f.randomCENCIV()
	opts := f.options(f.rest[0])
	opts.Progress = NewProgressBar()
	opts.OnDrop = func(d mp4.DroppedTrack) {
		fmt.Printf("dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
	}
	err := mp4.RemuxToABR(context.Background(), f.rest, f.outDir, opts)
	ClearProgress()
	if err != nil {
		FatalErr(err)
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
	f.needCENCIV()
	what, sources := rest[0], rest[1:]

	opts := f.options(sources[0])
	opts.OnDrop = func(d mp4.DroppedTrack) {
		fmt.Fprintf(os.Stderr, "dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
	}
	plan, err := mp4.PlanABR(context.Background(), sources, opts)
	if err != nil {
		FatalErr(err)
	}
	data, _, rerr := plan.Resource(context.Background(), what)
	if rerr != nil {
		Fatal(rerr.Error())
	}

	if outPath == "" || outPath == "-" {
		if _, err := os.Stdout.Write(data); err != nil {
			FatalErr(err)
		}
		return
	}
	guardOutput(outPath, sources...)
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		FatalErr(err)
	}
	fmt.Fprintf(os.Stderr, "%s → %s (%d bytes, %d variants)\n", what, outPath, len(data), plan.NumVariants())
}
