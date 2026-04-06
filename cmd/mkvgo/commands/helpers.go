package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gravity-zero/mkvgo/matroska"
)

var JsonOutput bool

const (
	progressThrottle = 100 * time.Millisecond
	barWidth         = 30
)

var CmdUsage = map[string]string{
	"info":               "mkvgo info [-json] <file.mkv>",
	"tracks":             "mkvgo tracks [-json] <file.mkv>",
	"chapters":           "mkvgo chapters [-json] <file.mkv>",
	"attachments":        "mkvgo attachments [-json] <file.mkv>",
	"tags":               "mkvgo tags [-json] <file.mkv>",
	"probe":              "mkvgo probe [-json] <file.mkv>",
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
	"extract-subtitle":   "mkvgo extract-subtitle <file.mkv> -t <trackID> -o <out> [-format srt|ass]",
	"split":              "mkvgo split <file.mkv> -o <dir> [-chapters | -range 0-5000,5000-0]",
	"join":               "mkvgo join -o <out.mkv> <file1.mkv> <file2.mkv> ...",
}

func CmdHelp(cmd string) {
	if u, ok := CmdUsage[cmd]; ok {
		fmt.Fprintf(os.Stderr, "usage: %s\n", u)
	} else {
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
	}
}

func Fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
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

func PrintJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		Fatal(err.Error())
	}
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
