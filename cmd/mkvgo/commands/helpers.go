package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gravity-zero/mkvgo/httpfs"
	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mp4"
	"github.com/gravity-zero/mkvgo/s3fs"
)

var JsonOutput bool

// InBand is the global -in-band flag: the inspection commands complete a video
// track's description from its first sample where the header leaves it short
// (reader.WithInBandColourFallback / mp4.Options.InBandColour).
var InBand bool

// FrameRateReads is the global -fps-reads flag: the frame-rate estimate of a
// track without DefaultDuration may read the block headers it needs
// (reader.WithFrameRateReads).
var FrameRateReads bool

// Force is the global -f/--force flag: allow overwriting an existing output
// file. Without it, commands that write a new file refuse to clobber one.
var Force bool

// GuardOverwrite refuses to overwrite an existing output file unless the
// global -f/--force flag is set, so a typo cannot silently destroy a file.
func GuardOverwrite(path string) {
	if Force || path == "" || path == "-" {
		return
	}
	if _, err := os.Stat(path); err == nil {
		Fatal(fmt.Sprintf("%s already exists (use -f/--force to overwrite)", path))
	}
}

const (
	progressThrottle = 100 * time.Millisecond
	barWidth         = 30
)

var CmdUsage = map[string]string{
	"info":               "mkvgo info [-json] <file.mkv|.mp4|->",
	"tracks":             "mkvgo tracks [-json] <file.mkv|.mp4|->",
	"chapters":           "mkvgo chapters [-json] <file.mkv|.mp4|->",
	"attachments":        "mkvgo attachments [-json] <file.mkv|.mp4|->",
	"tags":               "mkvgo tags [-json] <file.mkv|.mp4|->",
	"probe":              "mkvgo probe [-json] [-in-band] [-fps-reads] <file.mkv|.mp4|-> (full head-only metadata; -json adds every derived field: aspect ratios, colour names, hdr_format, resolved_language, effective_sample_rate; -in-band reads one sample to complete colour / HDR10 static metadata the header left out)",
	"keyframes":          "mkvgo keyframes [-json] <file.mkv|.mp4>",
	"to-vtt":             "mkvgo to-vtt <subtitle.srt|.ass|.vtt> -o <out.vtt>",
	"validate":           "mkvgo validate [-json] [-strict] <file.mkv> (exit 1 on errors; -strict: warnings fail too)",
	"hash":               "mkvgo hash <file.mkv> [-o <out.mkv>] (stores per-track CONTENT_SHA256 tags; in-place without -o)",
	"verify":             "mkvgo verify [-json] <file.mkv|.mp4> (recompute content hashes, exit 1 on mismatch)",
	"compare":            "mkvgo compare [-json] [-blocks] <a.mkv|.mp4> <b.mkv|.mp4> [part2.mkv ...] (-blocks: per-track content hashes, MKV/WebM only; several files on the right = compare against their concatenation)",
	"demux":              "mkvgo demux <file.mkv> -o <dir> [-t trackID,... (default: all tracks)]",
	"mux":                "mkvgo mux -o <out.mkv|.mp4> <file:trackID> [<file:trackID> ...] (all MKV/WebM or all MP4/MOV)",
	"merge":              "mkvgo merge -o <out.mkv|.mp4> <file1> [<file2> ...] (all MKV/WebM or all MP4/MOV)",
	"merge-subtitle":     "mkvgo merge-subtitle <file.mkv|.mp4> -o <out> <subtitle> [-format srt|ass (default: from extension; MP4: srt only)] [-lang code (default: und)] [-name text]",
	"remove-track":       "mkvgo remove-track <file.mkv|.mp4> -o <out> -t <trackID,...>",
	"add-track":          "mkvgo add-track <file.mkv|.mp4> -o <out> <source:trackID> [-lang code] [-name text]",
	"edit":               "mkvgo edit <file.mkv|.mp4> -o <out> '<json>' (or - for stdin)",
	"edit-title":         "mkvgo edit-title <file.mkv|.mp4> -o <out> <title>",
	"edit-track":         "mkvgo edit-track <file.mkv|.mp4> -o <out> -t <id> [-lang x] [-name x] [-default|-no-default] [-forced|-no-forced]",
	"edit-inplace":       "mkvgo edit-inplace <file.mkv|.mp4> '<json>' (instant, no rewrite; DESTRUCTIVE: modifies the file itself)",
	"extract-attachment": "mkvgo extract-attachment <file.mkv> <attachmentID> -o <outfile>",
	"add-attachment":     "mkvgo add-attachment <file.mkv> -o <out.mkv> <attachment file> [-name text] [-mime type (default: sniffed)]",
	"set-chapters":       "mkvgo set-chapters <file.mkv|.mp4> -o <out> <chapters.txt> (OGM format: CHAPTER01=00:00:00.000 / CHAPTER01NAME=Intro)",
	"extract-chapters":   "mkvgo extract-chapters <file.mkv|.mp4> [-o <chapters.txt>] (OGM format, stdout by default)",
	"remove-attachment":  "mkvgo remove-attachment <file.mkv> -o <out.mkv> <attachmentID|name>",
	"extract-subtitle":   "mkvgo extract-subtitle <file.mkv|.mp4> -t <trackID> -o <out> [-format srt|ass|vtt|pgs (default: srt; pgs writes a directory of PNGs + cues.json)] [-index <index.mkvsix> (vtt/pgs, MKV only)] [--damage-bytes n (MKV: pass over a damaged region within n bytes, its cues lost; default 64 MiB)]",
	"subtitle-index":     "mkvgo subtitle-index <file.mkv> -o <index.mkvsix> [-t 4,5 (default: every subtitle track)] [-from-cues] [--damage-bytes n]",
	"split":              "mkvgo split <file.mkv|.mp4> -o <dir> [-chapters | -range 0-5:00,5:00-0 | -every 6:00] [-pattern part_%03d.mkv|.mp4 ({title} = chapter title)]",
	"join":               "mkvgo join -o <out.mkv|.mp4> <file1> <file2> ... (all MKV/WebM or all MP4/MOV)",
	"reindex":            "mkvgo reindex <input.mkv> [output.mkv] [--deep-verify] [--replace] [--keep-backup] [--resync] [--clean-cut] [--strict] [--rollback-delta <file>] (rebuild the seek index; --resync repairs corrupted regions surgically)",
	"reindex-inplace":    "mkvgo reindex-inplace <file.mkv> [--deep-verify] [--rollback] [--strict] [--rollback-delta <file>] (rebuild the seek index by patching the file itself, crash-safe journal)",
	"salvage":            "mkvgo salvage <in.mkv> <out.mkv> [--json] [--clean-cut] [--rollback-delta <file>] | mkvgo salvage <in.mkv> --dry-run [--json] [--clean-cut] (best-effort recovery copy of a damaged file; --dry-run maps the damage)",
	"rollback":           "mkvgo rollback <repaired.mkv> <delta.rbd> <restored.mkv> (reconstruct the pre-repair original from its --rollback-delta entry)",
	"retime":             "mkvgo retime <file.mkv|.mp4> --shift <track>=<ms> [--shift ...] [--in-place | --replace] [--keep-backup] [--deep-verify] [--strict] [--rollback-delta <file>] (cancel a constant A/V desync; MP4 = moov edit list only, mode flags do not apply)",
	"track-ends":         "mkvgo track-ends <file.mkv> [-json] (where each track's content really ends: statistics tags when they describe the file, else a bounded header-only tail walk; picture end and audio shortfall)",
	"cue-health":         "mkvgo cue-health <file.mkv> [-json] [-probe] (head-only seek-index triage: which tracks the cues reference; -probe looks inside each hole; exit 1 when unhealthy)",
	"diagnose":           "mkvgo diagnose <file.mkv|.mp4> [--deep-verify] [-json] (one-call triage with a remedy per finding; MKV: index health + audio delay + size coherence; MP4: box layout + edit-list delays; exit 1 on findings)",
	"serve":              "mkvgo serve <file.mkv> [-addr 127.0.0.1:8478] [--direct | --auto [-target mse-generic]] [--window-cache <MiB>|off] [--window-bytes <MiB>|off] [--max-walks N] [--max-in-flight N] [--max-waiting N] [--memory-limit <MiB>] [--buffered] [--cors] [--source-access auto|local|network] (serve one file's on-demand HLS plan over HTTP, or the raw file for a direct-play client)",
	"serve-growing":      "mkvgo serve-growing <file.mkv> [-addr 127.0.0.1:8478] [-segment 6] [--cors] (play while downloading: serve a still-growing file as HLS, EVENT playlist until it finishes)",
	"to-mp4":             "mkvgo to-mp4 [--faststart] [--skip-unsupported] [--flatten-subs] [--webvtt-native] [--mp3-container-delay] [--hash] <input.mkv|.mp4> <output.mp4> (an MP4/MOV input is rewritten fast start, sample for sample)",
	"from-mp4":           "mkvgo from-mp4 [--skip-unsupported] [--mp3-container-delay] <input.mp4> <output.mkv>",
	"to-webm":            "mkvgo to-webm <input.mkv> <output.webm>",
	"to-hls":             "mkvgo to-hls <input.mkv> -o <dir> [-segment 6] [--skip-unsupported] [--keep-tracks 1,2 | --keep-lang fre] [--sub-offset ms] [--audio-shift track=ms] [--chapter-markers] [--no-iframes] (fragmented-MP4 HLS; virtual track subset by ID or language; virtual subtitle/audio resync; opt-in chapter/ad-insertion markers)",
	"hls-segment":        "mkvgo hls-segment <input.mkv|url> <master|playlist|init|N|resource-name> [-o out] [-segment 6] [--keep-tracks 1,2 | --keep-lang fre] [--sub-offset ms] [--audio-shift track=ms] [--synthesize-index] [--chapter-markers] [--no-iframes] [--damage-bytes n] (build one HLS resource on demand; --synthesize-index serves a no-Cue source by walking it once)",
	"to-abr":             "mkvgo to-abr -o <dir> <best.mkv> <lower.mkv> [...] [-segment 6] [--keep-tracks 1,2 | --keep-lang fre] [--sub-offset ms] [--chapter-markers] [--no-iframes] (multi-variant HLS master from pre-encoded qualities)",
	"abr-segment":        "mkvgo abr-segment <master.m3u8|v{k}/name> <best> <lower> [...] [-o out] [-segment 6] [--keep-tracks 1,2 | --keep-lang fre] [--sub-offset ms] [--chapter-markers] [--no-iframes] [--damage-bytes n] (build one ABR resource on demand)",
	"cmaf-mpd":           "mkvgo cmaf-mpd -o <manifest.mpd> [<rung-dir> ...] [--audio <dir>]... (DASH manifest over CMAF rungs an external encoder produced: each dir holds one rendition's init.mp4 + .m4s segments; rungs are checked segment-aligned first)",
	"cmaf-hls":           "mkvgo cmaf-hls -o <dir> [<rung-dir> ...] [--audio <dir>]... (HLS master + one media playlist per rendition over CMAF rungs an external encoder produced; same rung directories as cmaf-mpd)",
	"watermark-segment":  "mkvgo watermark-segment <a.mkv> <b.mkv> <master|playlist|init|N> [--variant A|B] [--pattern <hex>] [-o out] [-segment 6] (serve one resource of an A/B session-watermarked stream; segment N drawn from A/B per --variant or bit N of --pattern)",
	"forensic-segment":   "mkvgo forensic-segment <src.mkv|mp4> <master|playlist|init|N> [--variant A|B] [--pattern <hex>] [--distinct] [-o out] [-segment 6] (single-source A/B watermark: variant B drops one disposable H.264 frame per segment, timing-compensated; --distinct tells whether segment N carries a bit. Experimental session marking, robust neither to collusion nor to re-encoding)",
	"concat-hls":         "mkvgo concat-hls <in1> <in2> [...] -o <dir> [-segment 6] [--keep-tracks 1,2 | --keep-lang fre] [--sub-offset ms] (concatenate sources into one continuous HLS session)",
	"concat-segment":     "mkvgo concat-segment <master.m3u8|p{k}/name> <in1> <in2> [...] [-o out] [-segment 6] [--keep-tracks 1,2 | --keep-lang fre] [--sub-offset ms] (build one concat resource on demand)",
	"extract-frame":      "mkvgo extract-frame <file.mkv> <time> -o <out.h264|.hevc|.ivf> (keyframe nearest <time>, decoder-ready)",
	"extract-frames":     "mkvgo extract-frames <file.mkv|.mp4|url> (-every <time> | -at <t1,t2,...>) [-start <time>] [-end <time>] [-json] -o <out.mkv|-> (keyframe-only Matroska stream; stats on stderr)",
	"analyze":            "mkvgo analyze [-json] <file.mkv|url> (per-track frame/keyframe counts, bitrate, GOP, duration, cfr/vfr - walks every block header: reads about the whole file, no decode)",
	"fingerprint":        "mkvgo fingerprint [-json] <file.mkv|.mp4|url> (container-independent content identity: Presentation hash + per-track payload SHA-256, full read)",
	"playability":        "mkvgo playability [-target safari|chrome|firefox|chromecast-gen3|mse-generic|chromium-generic|brave|opera|vivaldi|samsung-internet|edge] [-json] <file.mkv|.mp4|url> (default target: mse-generic)",
	"ladder":             "mkvgo ladder [-json] <file.mkv|.mp4|url> (recommended ABR rungs, capped at the source resolution/bitrate)",
	"ingest":             "mkvgo ingest [-target name] [-reindex] [-analyze] [-json] <file.mkv|.mp4|url> (one-call serving plan: direct-play/remux-hls/transcode, default target: mse-generic)",
}

func CmdHelp(cmd string) {
	if u, ok := CmdUsage[cmd]; ok {
		fmt.Fprintf(os.Stderr, "usage: %s\n", u)
	} else {
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
	}
}

// maxSegmentSeconds bounds -segment: a day, far past any segment, so the milliseconds always fit.
const maxSegmentSeconds = 86400

// parseSegmentSeconds reads a -segment duration in seconds (the HLS convention) as milliseconds, at least one.
func parseSegmentSeconds(v string) int64 {
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil || !(secs >= 0.001 && secs <= maxSegmentSeconds) {
		Fatal(fmt.Sprintf("invalid -segment duration %q (seconds, at least 0.001 and at most %d)", v, maxSegmentSeconds))
	}
	return int64(secs * 1000)
}

// osExit is the process-exit hook Fatal uses. It is a variable so tests can
// override it (recovering from a panic) to exercise the CLI error paths
// in-process; in production it is os.Exit.
var osExit = os.Exit

func Fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	osExit(1)
}

// FatalErr is Fatal with err's message, naming the command that repairs a damaged file when err is one.
func FatalErr(err error) {
	msg := err.Error()
	if errors.Is(err, reader.ErrDamagedRegion) {
		msg += "\nrepair it with `mkvgo reindex --resync`"
	}
	Fatal(msg)
}

// rejectFlagArg fails with a clear "unknown flag" error when a positional argument
// looks like an option (starts with '-'), so a mistyped flag fails loudly instead of
// being treated as a filename. A lone "-" (conventionally stdin) is allowed.
func rejectFlagArg(a string) {
	if len(a) > 1 && a[0] == '-' {
		Fatal("unknown flag: " + a)
	}
}

func RequireArgs(args []string, n int, usage string) {
	if len(args) < n {
		Fatal("usage: " + usage)
	}
}

// isRemoteURL reports whether path is a remote source this CLI can read
// directly without downloading it first: an http(s) URL (httpfs) or an
// s3://bucket/key reference (s3fs).
func isRemoteURL(path string) bool {
	return httpfs.IsURL(path) || s3fs.IsURL(path)
}

// remotePort returns the mkv.FS port for a remote URL (http(s) or s3://).
// s3:// credentials/region/endpoint come from the standard AWS environment
// variables (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN,
// AWS_REGION or AWS_DEFAULT_REGION, AWS_ENDPOINT_URL) - see s3fs.Options.
func remotePort(url string) *mkv.FS {
	if s3fs.IsURL(url) {
		return s3fs.New(s3fs.Options{}).Port()
	}
	return httpfs.New().Port()
}

// s3Hybrid mirrors httpfs.Hybrid for s3:// sources: reads go through s3fs,
// every other path (including all writes) goes to the operating system.
func s3Hybrid() *mkv.FS {
	remote := s3fs.New(s3fs.Options{}).Port()
	return &mkv.FS{
		Open: func(path string) (mkv.ReadSeekCloser, error) {
			if s3fs.IsURL(path) {
				return remote.DoOpen(path)
			}
			return os.Open(path)
		},
		Stat: func(path string) (os.FileInfo, error) {
			if s3fs.IsURL(path) {
				return remote.DoStat(path)
			}
			return os.Stat(path)
		},
	}
}

func OpenMKV(path string) *matroska.Container {
	if isRemoteURL(path) {
		Fatal("this command needs the full local file; http(s)/s3:// sources are supported by the inspection commands (info, tracks, probe, keyframes, chapters, tags, attachments)")
	}
	c, err := matroska.Open(context.Background(), path)
	if err != nil {
		FatalErr(err)
	}
	return c
}

// openInput opens the MKV container from path. When path is "-" it reads from
// os.Stdin via ReadStream (forward-only; Cues are not populated). For any other
// path the existing seekable reader is used so behaviour is identical to before.
func openInput(path string) *matroska.Container {
	if path == "-" {
		c, _, err := reader.ReadStream(context.Background(), os.Stdin)
		if err != nil {
			FatalErr(err)
		}
		c.Path = "<stdin>"
		return c
	}
	return OpenMKV(path)
}

// isMP4Path reports whether path looks like an ISO-BMFF file the mp4 package
// handles (by extension; query/fragment suffixes of a URL are ignored).
func isMP4Path(path string) bool {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v", ".m4a", ".mov":
		return true
	}
	return false
}

// isMP4Content sniffs the file's first bytes for an ISO-BMFF box structure -
// the extension never decides, so a mislabeled file routes correctly. Used by
// the commands whose two engines write differently (retime, diagnose); read
// errors return false and let the Matroska engine report them.
func isMP4Content(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 8)
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	return mp4.OpensLikeISOBMFF(head)
}

// loadContainer reads metadata from path, handling MP4 (mp4.OpenMeta) as well as
// Matroska/WebM. keyframes requests the MP4 keyframe index (which builds the
// sample table); for MKV the keyframe index is filled from the Cues regardless.
// The second return is any non-carried MP4 tracks (cover art, hint/timecode).
func loadContainer(path string, keyframes bool) (*matroska.Container, []mp4.DroppedTrack) {
	if isRemoteURL(path) {
		return loadRemote(path, keyframes)
	}
	if path != "-" && isMP4Path(path) {
		return loadMP4Meta(path, keyframes)
	}
	if path == "-" {
		return openInput(path), nil
	}
	// A local path with a non-MP4 extension. Try Matroska first; if the bytes
	// are actually ISO base media (a .mkv that is really an MP4/MOV - mislabeled
	// rips happen), route to the mp4 reader transparently instead of failing
	// with a cryptic EBML error.
	c, err := matroska.Open(context.Background(), path, append(globalReadOptions(), matroska.WithoutAttachmentData())...) // the listing, not the payloads
	if err != nil {
		if errors.Is(err, matroska.ErrNotMatroska) {
			return loadMP4Meta(path, keyframes)
		}
		FatalErr(err)
	}
	return c, nil
}

// globalReadOptions are the Matroska read options the -in-band and -fps-reads flags add.
func globalReadOptions() []matroska.ReadOption {
	var o []matroska.ReadOption
	if InBand {
		o = append(o, matroska.WithInBandColourFallback(), matroska.WithInBandChannels())
	}
	if FrameRateReads {
		o = append(o, matroska.WithFrameRateReads())
	}
	return o
}

// loadMP4Meta reads MP4/MOV metadata (mp4.OpenMeta), optionally building the
// keyframe index. Shared by the extension dispatch and the mislabeled-container
// fallback in loadContainer.
func loadMP4Meta(path string, keyframes bool) (*matroska.Container, []mp4.DroppedTrack) {
	c, dropped, err := mp4.OpenMeta(context.Background(), path, mp4.Options{Keyframes: keyframes, InBandColour: InBand})
	if err != nil {
		FatalErr(err)
	}
	return c, dropped
}

// sourceFS returns the FS for a remux whose source may be a remote URL: the
// hybrid port (remote reads ranged, local paths and all writes on the OS), or
// nil (pure OS) for a local source. A remux reads sequentially, so a remote
// source amounts to a streamed download. Supports both http(s):// (httpfs)
// and s3:// (s3fs; credentials/region/endpoint from the AWS environment).
func sourceFS(src string) *mkv.FS {
	switch {
	case s3fs.IsURL(src):
		return s3Hybrid()
	case httpfs.IsURL(src):
		return httpfs.Hybrid()
	default:
		return nil
	}
}

// loadRemote reads a remote file's metadata over ranged reads - the head-only
// probe, so only a few ranged kilobytes are transferred whatever the file
// size. The MP4 probe is fully head-only (chapters/tags included); a remote
// Matroska read takes Info, Tracks, chapters, tags and the attachment list (no
// payload), and its keyframes from the Cues alone: without Cues the index is a
// walk of every Cluster, a full download. url may be an http(s):// or an
// s3://bucket/key reference.
func loadRemote(url string, keyframes bool) (*matroska.Container, []mp4.DroppedTrack) {
	fs := remotePort(url)
	if isMP4Path(url) {
		c, dropped, err := mp4.OpenMeta(context.Background(), url,
			mp4.Options{Keyframes: keyframes, InBandColour: InBand, FS: fs})
		if err != nil {
			FatalErr(err)
		}
		return c, dropped
	}
	ro := append(globalReadOptions(), matroska.WithChapters(), matroska.WithTags(),
		matroska.WithAttachments(), matroska.WithoutAttachmentData())
	c, err := matroska.OpenMetaWithFS(context.Background(), url, fs, ro...)
	if err != nil {
		FatalErr(err)
	}
	fmt.Fprintln(os.Stderr, "note: remote Matroska reads are metadata-only (keyframes from the Cues, attachment payloads stay remote)")
	return c, nil
}

func PrintJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		FatalErr(err)
	}
}

// trackJSON augments a Track for JSON output with the derived display strings
// a stream prober consumes as fields but which the library exposes as
// methods: codec/channel names, aspect ratios, the colour code points as
// names, and the one-word HDR classification a tonemap-or-direct-play
// decision keys on. The embedded Track's fields are promoted, so the JSON
// shape is the Track plus these extras.
type trackJSON struct {
	matroska.Track
	// FrameRate is the Track's, else its EstimatedFrameRate with FrameRateEstimated set.
	FrameRate           *float64 `json:"frame_rate,omitempty"`
	FrameRateEstimated  bool     `json:"frame_rate_estimated,omitempty"`
	EstimatedFrameRate  *float64 `json:"estimated_frame_rate,omitempty"` // folded into frame_rate
	CodecName           string   `json:"codec_name,omitempty"`
	CodecLongName       string   `json:"codec_long_name,omitempty"`
	ChannelLayout       string   `json:"channel_layout,omitempty"`
	AvgFrameRate        float64  `json:"avg_frame_rate,omitempty"`
	SampleAspectRatio   string   `json:"sample_aspect_ratio,omitempty"`
	DisplayAspectRatio  string   `json:"display_aspect_ratio,omitempty"`
	ColorSpaceName      string   `json:"color_space_name,omitempty"`
	ColorTransferName   string   `json:"color_transfer_name,omitempty"`
	ColorPrimariesName  string   `json:"color_primaries_name,omitempty"`
	ColorRangeName      string   `json:"color_range_name,omitempty"`
	HDRFormat           string   `json:"hdr_format,omitempty"`
	StereoModeName      string   `json:"stereo_mode_name,omitempty"`
	ResolvedLanguage    string   `json:"resolved_language,omitempty"`
	EffectiveSampleRate float64  `json:"effective_sample_rate,omitempty"`
}

// tracksForJSON wraps tracks so the marshaled JSON carries the derived display
// fields alongside the raw values.
func tracksForJSON(c *matroska.Container) []trackJSON {
	out := make([]trackJSON, len(c.Tracks))
	for i, t := range c.Tracks {
		out[i] = trackJSON{Track: t,
			CodecName:           c.TrackCodecName(&t),
			CodecLongName:       t.CodecLongName(),
			ChannelLayout:       t.ChannelLayout(),
			AvgFrameRate:        t.AvgFrameRate(),
			SampleAspectRatio:   t.SampleAspectRatio(),
			DisplayAspectRatio:  t.DisplayAspectRatio(),
			ColorSpaceName:      t.ColorSpaceName(),
			ColorTransferName:   t.ColorTransferName(),
			ColorPrimariesName:  t.ColorPrimariesName(),
			ColorRangeName:      t.ColorRangeName(),
			HDRFormat:           t.HDRFormat(),
			StereoModeName:      t.StereoModeName(),
			ResolvedLanguage:    c.TrackLanguage(&t),
			EffectiveSampleRate: t.EffectiveSampleRate(),
		}
		out[i].FrameRate, out[i].FrameRateEstimated = t.FrameRate, t.FrameRate == nil && t.EstimatedFrameRate != nil
		if out[i].FrameRateEstimated {
			out[i].FrameRate = t.EstimatedFrameRate
		}
	}
	return out
}

// containerForJSON wraps a container so its tracks carry the derived display
// fields. The outer Tracks field shadows the embedded container's in the JSON.
func containerForJSON(c *matroska.Container) any {
	return struct {
		*matroska.Container
		Tracks []trackJSON `json:"tracks"`
	}{Container: c, Tracks: tracksForJSON(c)}
}

// splitTrackSpec splits a "file:trackID" argument on its LAST colon, so a path
// that itself contains a colon survives - most importantly a Windows
// drive-letter path (C:\dir\file.mkv:1). ok is false when there is no separator
// leaving a non-empty path on the left and a non-empty trackID on the right.
func splitTrackSpec(spec string) (path, trackID string, ok bool) {
	sep := strings.LastIndex(spec, ":")
	if sep <= 0 || sep == len(spec)-1 {
		return "", "", false
	}
	return spec[:sep], spec[sep+1:], true
}

func ParseTrackIDs(s string) []uint64 {
	var ids []uint64
	for _, part := range strings.Split(s, ",") {
		id, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64)
		if err != nil {
			Fatal(fmt.Sprintf("invalid track ID %q: %v", part, err))
		}
		ids = append(ids, id)
	}
	return ids
}

func ParseTimeRanges(s string) []matroska.TimeRange {
	var ranges []matroska.TimeRange
	for _, part := range strings.Split(s, ",") {
		parts := strings.SplitN(part, "-", 2)
		if len(parts) != 2 {
			Fatal(fmt.Sprintf("invalid range %q, expected start-end", part))
		}
		start, err := ParseTimePoint(parts[0])
		if err != nil {
			Fatal(fmt.Sprintf("invalid start time %q: %v", parts[0], err))
		}
		end, err := ParseTimePoint(parts[1])
		if err != nil {
			Fatal(fmt.Sprintf("invalid end time %q: %v", parts[1], err))
		}
		ranges = append(ranges, matroska.TimeRange{StartMs: start, EndMs: end})
	}
	return ranges
}

// ParseTimePoint parses one range bound into milliseconds. Accepted forms:
// plain integer milliseconds ("300000"), seconds with a fraction ("90.5"),
// or a clock time [HH:]MM:SS[.fraction] ("5:00", "01:30:00", "1:30.5").
func ParseTimePoint(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if !strings.ContainsAny(s, ":.") {
		return strconv.ParseInt(s, 10, 64) // milliseconds
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("expected [HH:]MM:SS[.fraction]")
	}
	// The last field carries the seconds (possibly fractional); the fields
	// before it are minutes, then hours.
	sec, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil || sec < 0 {
		return 0, fmt.Errorf("invalid seconds %q", parts[len(parts)-1])
	}
	total := sec
	for i, mult := len(parts)-2, float64(60); i >= 0; i, mult = i-1, mult*60 {
		v, err := strconv.ParseInt(parts[i], 10, 64)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("invalid time component %q", parts[i])
		}
		total += float64(v) * mult
	}
	ms := math.Round(total * 1000) // 1.001 s is 1000.9999... ms in binary floating point: rounded, not truncated
	if ms > math.MaxInt64/2 {
		return 0, fmt.Errorf("time %q is out of range", s)
	}
	return int64(ms), nil
}

func FmtMs(ms int64) string {
	s := ms / 1000
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}

func NewProgressBar() matroska.ProgressFunc {
	var mu sync.Mutex
	var lastPrint time.Time

	return func(processed, total int64) {
		mu.Lock()
		defer mu.Unlock()

		if time.Since(lastPrint) < progressThrottle {
			return
		}
		lastPrint = time.Now()

		if total <= 0 {
			fmt.Fprintf(os.Stderr, "\r  %s processed", FormatBytes(processed))
			return
		}

		pct := float64(processed) / float64(total) * 100
		if pct > 100 {
			pct = 100
		}
		filled := int(pct / 100 * float64(barWidth))
		bar := strings.Repeat("=", filled) + strings.Repeat(" ", barWidth-filled)
		fmt.Fprintf(os.Stderr, "\r  [%s] %5.1f%% %s/%s", bar, pct, FormatBytes(processed), FormatBytes(total))
	}
}

// writeFileFrom runs write against a buffer and stores the result at path only
// once it has succeeded: a refused request neither leaves an empty file under
// the output name nor empties one already there. For small text outputs only
// (subtitles) - the whole result is held in memory.
func writeFileFrom(path string, write func(io.Writer) error) error {
	var buf bytes.Buffer
	if err := write(&buf); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func ClearProgress() {
	fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", 70))
}

func FormatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// needValue advances *i to the value of the flag at args[*i] and returns it, exiting when the flag is the last argument.
func needValue(args []string, i *int) string {
	flag := args[*i]
	*i++
	if *i >= len(args) {
		Fatal(flag + " needs a value")
	}
	return args[*i]
}
