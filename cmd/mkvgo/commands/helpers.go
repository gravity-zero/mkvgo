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

const (
	progressThrottle = 100 * time.Millisecond
	barWidth         = 30
)

var CmdUsage = map[string]string{
	"info":               "mkvgo info [-json] <file.mkv|.mp4|->",
	"tracks":             "mkvgo tracks [-json] <file.mkv|.mp4|->",
	"chapters":           "mkvgo chapters [-json] <file.mkv|.mp4|->",
	"attachments":        "mkvgo attachments [-json] <file.mkv|->",
	"tags":               "mkvgo tags [-json] <file.mkv|->",
	"probe":              "mkvgo probe [-json] <file.mkv|.mp4|->",
	"keyframes":          "mkvgo keyframes [-json] <file.mkv|.mp4>",
	"to-vtt":             "mkvgo to-vtt <subtitle.srt|.ass|.vtt> -o <out.vtt>",
	"validate":           "mkvgo validate [-json] <file.mkv>",
	"compare":            "mkvgo compare [-json] <a.mkv> <b.mkv>",
	"demux":              "mkvgo demux <file.mkv> -o <dir> [-t trackID,...]",
	"mux":                "mkvgo mux -o <out.mkv> <file:trackID> [<file:trackID> ...]",
	"merge":              "mkvgo merge -o <out.mkv> <file1.mkv> [<file2.mkv> ...]",
	"merge-subtitle":     "mkvgo merge-subtitle <file.mkv> -o <out.mkv> <subtitle> [-format srt|ass] [-lang code] [-name text]",
	"remove-track":       "mkvgo remove-track <file.mkv> -o <out.mkv> -t <trackID,...>",
	"add-track":          "mkvgo add-track <file.mkv> -o <out.mkv> <source:trackID> [-lang code] [-name text]",
	"edit":               "mkvgo edit <file.mkv> -o <out.mkv> '<json>' (or - for stdin)",
	"edit-title":         "mkvgo edit-title <file.mkv> -o <out.mkv> <title>",
	"edit-track":         "mkvgo edit-track <file.mkv> -o <out.mkv> -t <id> [-lang x] [-name x] [-default|-no-default] [-forced|-no-forced]",
	"edit-inplace":       "mkvgo edit-inplace <file.mkv> '<json>' (instant, no rewrite)",
	"extract-attachment": "mkvgo extract-attachment <file.mkv> <attachmentID> -o <outfile>",
	"extract-subtitle":   "mkvgo extract-subtitle <file.mkv|.mp4> -t <trackID> -o <out> [-format srt|ass|vtt]",
	"split":              "mkvgo split <file.mkv> -o <dir> [-chapters | -range 0-5000,5000-0]",
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
		start, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil {
			Fatal(fmt.Sprintf("invalid start time %q", parts[0]))
		}
		end, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			Fatal(fmt.Sprintf("invalid end time %q", parts[1]))
		}
		ranges = append(ranges, matroska.TimeRange{StartMs: start, EndMs: end})
	}
	return ranges
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
