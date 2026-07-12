package subtitle

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// webvtt.go - WebVTT serialization. A Cue is the timed-text unit; WriteWebVTT
// emits a valid WebVTT file from cues, and the converters turn the package's
// parsed SRT/ASS into cues. FileToWebVTT is the file-level entry point that
// replaces an external conversion fork for subtitle sidecars.

// Cue is a single WebVTT cue.
type Cue struct {
	StartMs  int64
	EndMs    int64
	Settings string // optional cue settings (e.g. "line:90%"); "" for none
	Text     string // payload, may contain newlines
}

// FormatVTTTime formats a millisecond timestamp as a WebVTT time HH:MM:SS.mmm.
func FormatVTTTime(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	h := ms / mkv.MsPerHour
	m := (ms % mkv.MsPerHour) / mkv.MsPerMinute
	s := (ms % mkv.MsPerMinute) / mkv.MsPerSecond
	milli := ms % mkv.MsPerSecond
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, milli)
}

// WriteWebVTT writes cues as a WebVTT file. Cues with empty text are skipped; the
// rest are written in order with a "WEBVTT" header. Inline markup (<i>, <b>, …) is
// kept verbatim - it is valid in both SRT and WebVTT.
func WriteWebVTT(w io.Writer, cues []Cue) error {
	bw := bufio.NewWriter(w)
	if _, err := bw.WriteString("WEBVTT\n\n"); err != nil {
		return err
	}
	for _, c := range cues {
		text := strings.TrimRight(c.Text, "\n")
		if text == "" {
			continue
		}
		timing := FormatVTTTime(c.StartMs) + " --> " + FormatVTTTime(c.EndMs)
		if c.Settings != "" {
			timing += " " + c.Settings
		}
		if _, err := fmt.Fprintf(bw, "%s\n%s\n\n", timing, text); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// SRTToCues converts parsed SRT entries to WebVTT cues. SRT and WebVTT share the
// same cue-body syntax, so the text passes through unchanged.
func SRTToCues(entries []SRTEntry) []Cue {
	cues := make([]Cue, 0, len(entries))
	for _, e := range entries {
		cues = append(cues, Cue{StartMs: e.StartMs, EndMs: e.EndMs, Text: e.Text})
	}
	return cues
}

// ASSToCues converts parsed ASS dialogue events to WebVTT cues, reducing each to
// plain text: override blocks ({\...}) are stripped and the ASS escapes \N/\n/\h
// become newlines/spaces. Styling and positioning are discarded (lossy).
func ASSToCues(events []ASSEvent) []Cue {
	cues := make([]Cue, 0, len(events))
	for _, e := range events {
		cues = append(cues, Cue{StartMs: e.StartMs, EndMs: e.EndMs, Text: assEventText(e.Fields)})
	}
	return cues
}

// assEventText extracts the dialogue text from an external ASS event's fields
// (Style,Name,MarginL,MarginR,MarginV,Effect,Text) and flattens it to plain text.
func assEventText(fields string) string {
	parts := strings.SplitN(fields, ",", 7)
	if len(parts) < 7 {
		return ""
	}
	return flattenASSText(parts[6])
}

// FlattenASSBlock flattens a Matroska S_TEXT/ASS block payload
// (ReadOrder,Layer,Style,Name,MarginL,MarginR,MarginV,Effect,Text) to plain text.
// Used when extracting an embedded ASS subtitle track to WebVTT.
func FlattenASSBlock(data []byte) string {
	parts := strings.SplitN(string(data), ",", 9)
	if len(parts) < 9 {
		return ""
	}
	return flattenASSText(parts[8])
}

// flattenASSText strips ASS override blocks and converts the ASS line escapes.
func flattenASSText(s string) string {
	s = stripASSOverride(s)
	s = strings.ReplaceAll(s, `\N`, "\n")
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\h`, " ")
	return strings.TrimSpace(s)
}

// stripASSOverride removes ASS override blocks delimited by { and }.
func stripASSOverride(s string) string {
	if !strings.ContainsRune(s, '{') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	depth := 0
	for _, r := range s {
		switch r {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// ShiftCues returns cues with every timestamp shifted by offsetMs (positive or
// negative) - a virtual subtitle resync: no source is rewritten, the shift is
// applied wherever cues are about to be rendered or windowed. A cue whose
// shifted end lands at or before 0 is dropped (it would never appear on
// screen); a cue straddling 0 is clamped to start at 0 (some of it still
// airs). offsetMs == 0 returns cues unchanged (same slice, same order), so
// the zero-offset path stays byte-identical to before this existed.
func ShiftCues(cues []Cue, offsetMs int64) []Cue {
	if offsetMs == 0 {
		return cues
	}
	out := make([]Cue, 0, len(cues))
	for _, c := range cues {
		c.StartMs += offsetMs
		c.EndMs += offsetMs
		if c.EndMs <= 0 {
			continue
		}
		if c.StartMs < 0 {
			c.StartMs = 0
		}
		out = append(out, c)
	}
	return out
}

// ResolveCueEnds fills any cue whose end is unset (<= its start, e.g. no source
// duration) from the next cue's start, falling back to defaultDurMs for the last
// cue. Cues must already be in start order.
func ResolveCueEnds(cues []Cue, defaultDurMs int64) {
	for i := range cues {
		if cues[i].EndMs > cues[i].StartMs {
			continue
		}
		if i+1 < len(cues) && cues[i+1].StartMs > cues[i].StartMs {
			cues[i].EndMs = cues[i+1].StartMs
		} else {
			cues[i].EndMs = cues[i].StartMs + defaultDurMs
		}
	}
}

// FileToWebVTT reads an external subtitle file and writes it as WebVTT to w. The
// format is detected from the extension: .srt, .ass/.ssa, or .vtt (already WebVTT,
// streamed through). It replaces an external conversion fork for sidecars.
func FileToWebVTT(srcPath string, w io.Writer) error {
	switch strings.ToLower(filepath.Ext(srcPath)) {
	case ".vtt":
		f, err := os.Open(srcPath)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	case ".ass", ".ssa":
		ass, err := ParseASS(srcPath)
		if err != nil {
			return fmt.Errorf("parse ASS: %w", err)
		}
		return WriteWebVTT(w, ASSToCues(ass.Events))
	case ".srt", "":
		entries, err := ParseSRT(srcPath)
		if err != nil {
			return fmt.Errorf("parse SRT: %w", err)
		}
		return WriteWebVTT(w, SRTToCues(entries))
	default:
		return fmt.Errorf("unsupported subtitle file extension %q", filepath.Ext(srcPath))
	}
}
