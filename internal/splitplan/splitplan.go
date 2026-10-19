// Package splitplan holds the split rules shared by the MP4 and Matroska engines: ranges, part names, chapter clipping.
package splitplan

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gravity-zero/mkvgo/mkv"
)

// ByStart is chapters in the order they play, as a split by chapters cuts them: a file may list them in any order.
func ByStart(chapters []mkv.Chapter) []mkv.Chapter {
	out := slices.Clone(chapters)
	slices.SortStableFunc(out, func(a, b mkv.Chapter) int { return cmp.Compare(a.StartMs, b.StartMs) })
	return out
}

// ChapterRanges turns chapters into ranges covering the whole file: each runs at least to the next chapter in time, the
// first from 0, the last to the end.
func ChapterRanges(chapters []mkv.Chapter) []mkv.TimeRange {
	ranges := make([]mkv.TimeRange, len(chapters))
	next := nextStarts(chapters)
	first := int64(-1)
	for _, ch := range chapters {
		if first < 0 || ch.StartMs < first {
			first = ch.StartMs
		}
	}
	for i, ch := range chapters {
		ranges[i] = mkv.TimeRange{StartMs: ch.StartMs, EndMs: ch.EndMs}
		switch {
		case next[i] < 0:
			ranges[i].EndMs = 0 // what plays after the last chapter's stated end is in its part
		case ranges[i].EndMs < next[i]:
			ranges[i].EndMs = next[i] // a gap before the next chapter stays in this part
		}
		if ch.StartMs == first {
			ranges[i].StartMs = 0
		}
	}
	return ranges
}

// CheckRanges refuses a range that ends before it starts or starts past durationMs (0: unknown), naming it by number.
func CheckRanges(ranges []mkv.TimeRange, durationMs int64) error {
	for i, r := range ranges {
		if r.EndMs > 0 && r.EndMs <= r.StartMs {
			return fmt.Errorf("range %d ends at %d ms, before it starts (%d ms): give an end after the start, or none to run to the end", i+1, r.EndMs, r.StartMs)
		}
		if durationMs > 0 && r.StartMs >= durationMs {
			return fmt.Errorf("range %d starts at %d ms, past the end of the file (%d ms): start it earlier", i+1, r.StartMs, durationMs)
		}
	}
	return nil
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

// CheckPattern refuses a part-name pattern that is a path or whose verbs are not one integer: it names a file in the output directory.
func CheckPattern(pattern string) error {
	if strings.ContainsAny(pattern, `/\`) {
		return fmt.Errorf("pattern %q names a path: it names a file inside the output directory, e.g. part_%%03d%s", pattern, filepath.Ext(pattern))
	}
	if hasVerb(pattern) && strings.Contains(fmt.Sprintf(pattern, 1), "%!") {
		return fmt.Errorf("pattern %q: its only verb is the part number, e.g. %%d or %%03d (write %%%% for a literal %%)", pattern)
	}
	return nil
}

// hasVerb reports whether a pattern holds a verb besides literal %% signs.
func hasVerb(pattern string) bool {
	return strings.Contains(strings.ReplaceAll(pattern, "%%", ""), "%")
}

// PartName names part i (0-based) from pattern, title filling {title}; used holds the names taken, compared without case.
func PartName(pattern string, i int, title string, hasTitle bool, used map[string]bool) string {
	pat := pattern
	if hasTitle {
		pat = strings.ReplaceAll(pat, "{title}", SanitizeName(title, i+1))
	}
	name := strings.ReplaceAll(pat, "%%", "%")
	if hasVerb(pat) {
		name = fmt.Sprintf(pat, i+1)
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for k := i + 1; used[strings.ToLower(name)]; k++ {
		name = stem + fmt.Sprintf("_%d", k) + ext
	}
	used[strings.ToLower(name)] = true
	return name
}

// maxTitleBytes bounds the title a part name takes.
const maxTitleBytes = 200

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
	for len(s) > maxTitleBytes { // file systems cap a name near 255 bytes: keep room for the pattern
		_, size := utf8.DecodeLastRuneInString(s)
		s = strings.TrimRight(s[:len(s)-size], " .")
	}
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
