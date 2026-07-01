package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mp4"
)

var JsonOutput bool

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
	"probe":              "mkvgo probe [-json] <file.mkv|.mp4|->",
	"keyframes":          "mkvgo keyframes [-json] <file.mkv|.mp4>",
	"to-vtt":             "mkvgo to-vtt <subtitle.srt|.ass|.vtt> -o <out.vtt>",
	"validate":           "mkvgo validate [-json] [-strict] <file.mkv> (exit 1 on errors; -strict: warnings fail too)",
	"compare":            "mkvgo compare [-json] <a.mkv> <b.mkv>",
	"demux":              "mkvgo demux <file.mkv> -o <dir> [-t trackID,... (default: all tracks)]",
	"mux":                "mkvgo mux -o <out.mkv> <file:trackID> [<file:trackID> ...]",
	"merge":              "mkvgo merge -o <out.mkv> <file1.mkv> [<file2.mkv> ...]",
	"merge-subtitle":     "mkvgo merge-subtitle <file.mkv> -o <out.mkv> <subtitle> [-format srt|ass (default: from extension)] [-lang code (default: und)] [-name text]",
	"remove-track":       "mkvgo remove-track <file.mkv> -o <out.mkv> -t <trackID,...>",
	"add-track":          "mkvgo add-track <file.mkv> -o <out.mkv> <source:trackID> [-lang code] [-name text]",
	"edit":               "mkvgo edit <file.mkv> -o <out.mkv> '<json>' (or - for stdin)",
	"edit-title":         "mkvgo edit-title <file.mkv> -o <out.mkv> <title>",
	"edit-track":         "mkvgo edit-track <file.mkv> -o <out.mkv> -t <id> [-lang x] [-name x] [-default|-no-default] [-forced|-no-forced]",
	"edit-inplace":       "mkvgo edit-inplace <file.mkv> '<json>' (instant, no rewrite; DESTRUCTIVE: modifies <file.mkv> itself)",
	"extract-attachment": "mkvgo extract-attachment <file.mkv> <attachmentID> -o <outfile>",
	"extract-subtitle":   "mkvgo extract-subtitle <file.mkv|.mp4> -t <trackID> -o <out> [-format srt|ass|vtt (default: srt)]",
	"split":              "mkvgo split <file.mkv> -o <dir> [-chapters | -range 0-5:00,5:00-0] (bounds: ms, seconds.frac or [HH:]MM:SS)",
	"join":               "mkvgo join -o <out.mkv> <file1.mkv> <file2.mkv> ...",
	"reindex":            "mkvgo reindex <input.mkv> <output.mkv>",
	"to-mp4":             "mkvgo to-mp4 [--faststart] [--skip-unsupported] [--flatten-subs] [--webvtt-native] [--mp3-container-delay] <input.mkv> <output.mp4>",
	"from-mp4":           "mkvgo from-mp4 [--mp3-container-delay] <input.mp4> <output.mkv>",
	"to-webm":            "mkvgo to-webm <input.mkv> <output.webm>",
}

func CmdHelp(cmd string) {
	if u, ok := CmdUsage[cmd]; ok {
		fmt.Fprintf(os.Stderr, "usage: %s\n", u)
	} else {
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
	}
}

// osExit is the process-exit hook Fatal uses. It is a variable so tests can
// override it (recovering from a panic) to exercise the CLI error paths
// in-process; in production it is os.Exit.
var osExit = os.Exit

func Fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	osExit(1)
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

func OpenMKV(path string) *matroska.Container {
	c, err := matroska.Open(context.Background(), path)
	if err != nil {
		Fatal(err.Error())
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
			Fatal(err.Error())
		}
		c.Path = "<stdin>"
		return c
	}
	return OpenMKV(path)
}

// isMP4Path reports whether path looks like an ISO-BMFF file the mp4 package
// handles (by extension).
func isMP4Path(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v", ".m4a", ".mov":
		return true
	}
	return false
}

// loadContainer reads metadata from path, handling MP4 (mp4.OpenMeta) as well as
// Matroska/WebM. keyframes requests the MP4 keyframe index (which builds the
// sample table); for MKV the keyframe index is filled from the Cues regardless.
// The second return is any non-carried MP4 tracks (cover art, hint/timecode).
func loadContainer(path string, keyframes bool) (*matroska.Container, []mp4.DroppedTrack) {
	if path != "-" && isMP4Path(path) {
		var opts []mp4.Options
		if keyframes {
			opts = append(opts, mp4.Options{Keyframes: true})
		}
		c, dropped, err := mp4.OpenMeta(context.Background(), path, opts...)
		if err != nil {
			Fatal(err.Error())
		}
		return c, dropped
	}
	return openInput(path), nil
}

func PrintJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		Fatal(err.Error())
	}
}

// trackJSON augments a Track for JSON output with the derived display strings
// ffprobe reports as fields (codec_long_name, channel_layout) but which the
// library exposes as methods. The embedded Track's fields are promoted, so the
// JSON shape is the Track plus these two extras.
type trackJSON struct {
	matroska.Track
	CodecLongName string  `json:"codec_long_name,omitempty"`
	ChannelLayout string  `json:"channel_layout,omitempty"`
	AvgFrameRate  float64 `json:"avg_frame_rate,omitempty"`
}

// tracksForJSON wraps tracks so the marshaled JSON carries the derived display
// fields alongside the raw values.
func tracksForJSON(tracks []matroska.Track) []trackJSON {
	out := make([]trackJSON, len(tracks))
	for i, t := range tracks {
		out[i] = trackJSON{Track: t, CodecLongName: t.CodecLongName(), ChannelLayout: t.ChannelLayout(), AvgFrameRate: t.AvgFrameRate()}
	}
	return out
}

// containerForJSON wraps a container so its tracks carry the derived display
// fields. The outer Tracks field shadows the embedded container's in the JSON.
func containerForJSON(c *matroska.Container) any {
	return struct {
		*matroska.Container
		Tracks []trackJSON `json:"tracks"`
	}{Container: c, Tracks: tracksForJSON(c.Tracks)}
}

// splitTrackSpec splits a "file:trackID" argument on its LAST colon, so a path
// that itself contains a colon survives — most importantly a Windows
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
	return int64(total * 1000), nil
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
