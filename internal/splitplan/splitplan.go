// Package splitplan holds the split rules shared by the MP4 and Matroska engines: ranges, part names, chapter clipping.
package splitplan

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// ChapterRanges turns chapters into ranges, a chapter without an end running to the next one in time.
func ChapterRanges(chapters []mkv.Chapter) []mkv.TimeRange {
	ranges := make([]mkv.TimeRange, len(chapters))
	next := nextStarts(chapters)
	for i, ch := range chapters {
		ranges[i] = mkv.TimeRange{StartMs: ch.StartMs, EndMs: ch.EndMs}
		if ranges[i].EndMs == 0 && next[i] > 0 {
			ranges[i].EndMs = next[i]
		}
	}
	return ranges
}

// EveryRanges builds keyframe-aligned ranges of roughly everyMs each.
func EveryRanges(keyframes []int64, everyMs int64) ([]mkv.TimeRange, error) {
	if len(keyframes) == 0 {
		return nil, fmt.Errorf("no keyframe index to segment on")
	}
	var bounds []int64
	prev := int64(0)
	for _, k := range keyframes {
		if k >= prev+everyMs {
			bounds = append(bounds, k)
			prev = k
		}
	}
	ranges := make([]mkv.TimeRange, 0, len(bounds)+1)
	start := int64(0)
	for _, b := range bounds {
		ranges = append(ranges, mkv.TimeRange{StartMs: start, EndMs: b})
		start = b
	}
	return append(ranges, mkv.TimeRange{StartMs: start, EndMs: 0}), nil
}

// PartName names part i (0-based) from pattern, title filling {title}; used tracks the names already taken.
func PartName(pattern string, i int, title string, hasTitle bool, used map[string]bool) string {
	pat := pattern
	if hasTitle {
		pat = strings.ReplaceAll(pat, "{title}", SanitizeName(title, i+1))
	}
	name := pat
	if strings.Contains(pat, "%") {
		name = fmt.Sprintf(pat, i+1)
	}
	if used[name] {
		ext := filepath.Ext(name)
		name = strings.TrimSuffix(name, ext) + fmt.Sprintf("_%d", i+1) + ext
	}
	used[name] = true
	return name
}

// SanitizeName makes a chapter title safe as a file-name fragment ("chapter_<n>" when nothing is left).
func SanitizeName(title string, n int) string {
	var b strings.Builder
	for _, r := range title {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == ' ', r == '-', r == '_', r == '.', r == '(', r == ')', r > 127:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), " .")
	if s == "" {
		return fmt.Sprintf("chapter_%02d", n)
	}
	stem, _, _ := strings.Cut(s, ".")
	if DeviceName(strings.ToUpper(strings.TrimRight(stem, " "))) {
		s = "_" + s
	}
	return s
}

// DeviceName reports a name Windows reserves for a device: CON, PRN, AUX, NUL, COM and LPT followed by 0-9, ¹, ² or ³.
func DeviceName(s string) bool {
	switch s {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if !strings.HasPrefix(s, "COM") && !strings.HasPrefix(s, "LPT") {
		return false
	}
	switch d := s[3:]; d {
	case "¹", "²", "³":
		return true
	default:
		return len(d) == 1 && d[0] >= '0' && d[0] <= '9'
	}
}

// SafePath joins name under baseDir, refusing a name that escapes it.
func SafePath(baseDir, name string) (string, error) {
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("absolute path not allowed: %s", name)
	}
	if strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("path escapes base directory: %s", name)
	}
	full := filepath.Join(baseDir, clean)
	rel, err := filepath.Rel(baseDir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path escapes base directory: %s", name)
	}
	return full, nil
}

// ClipChapters keeps the chapters overlapping [startMs, endMs) rebased onto the part (endMs 0: to the end).
func ClipChapters(chapters []mkv.Chapter, startMs, endMs int64) []mkv.Chapter {
	var out []mkv.Chapter
	next := nextStarts(chapters)
	for i, ch := range chapters {
		if endMs > 0 && ch.StartMs >= endMs {
			continue
		}
		end := ch.EndMs
		if end == 0 {
			end = next[i]
		}
		if end > 0 && end <= startMs {
			continue
		}
		clipped := ch
		clipped.StartMs = max(ch.StartMs-startMs, 0)
		if ch.EndMs > 0 {
			end := ch.EndMs
			if endMs > 0 && end > endMs {
				end = endMs
			}
			clipped.EndMs = end - startMs
		}
		clipped.SubChapters = ClipChapters(ch.SubChapters, startMs, endMs)
		out = append(out, clipped)
	}
	return out
}

// nextStarts gives, per chapter, the start of the sibling that follows it in time (-1: none).
func nextStarts(chapters []mkv.Chapter) []int64 {
	sorted := make([]int64, len(chapters))
	for i, ch := range chapters {
		sorted[i] = ch.StartMs
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	out := make([]int64, len(chapters))
	for i, ch := range chapters {
		j := sort.Search(len(sorted), func(k int) bool { return sorted[k] > ch.StartMs })
		if j == len(sorted) {
			out[i] = -1
			continue
		}
		out[i] = sorted[j]
	}
	return out
}
