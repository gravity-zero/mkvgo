package mkv

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// chapters_ogm.go - the OGM "simple chapter" text format, the lingua franca
// chapter-aware tools exchange:
//
//	CHAPTER01=00:00:00.000
//	CHAPTER01NAME=Intro
//	CHAPTER02=00:05:12.500
//	CHAPTER02NAME=Part One

// ParseOGMChapters parses OGM simple-format chapters. Entries are ordered by
// their chapter number; each chapter's EndMs is the next chapter's start (the
// last one keeps EndMs 0 = "until the end"). Blank lines and lines starting
// with '#' or ';' are ignored.
func ParseOGMChapters(r io.Reader) ([]Chapter, error) {
	starts := map[int]int64{}
	names := map[int]string{}
	sc := bufio.NewScanner(r)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=value, got %q", lineNo, line)
		}
		key = strings.ToUpper(strings.TrimSpace(key))
		if !strings.HasPrefix(key, "CHAPTER") {
			return nil, fmt.Errorf("line %d: unknown key %q", lineNo, key)
		}
		rest := key[len("CHAPTER"):]
		if n, isName := strings.CutSuffix(rest, "NAME"); isName {
			num, err := strconv.Atoi(n)
			if err != nil {
				return nil, fmt.Errorf("line %d: bad chapter number in %q", lineNo, key)
			}
			names[num] = strings.TrimSpace(val)
			continue
		}
		num, err := strconv.Atoi(rest)
		if err != nil {
			return nil, fmt.Errorf("line %d: bad chapter number in %q", lineNo, key)
		}
		ms, err := parseOGMTime(strings.TrimSpace(val))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		starts[num] = ms
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(starts) == 0 {
		return nil, fmt.Errorf("no CHAPTERnn entries found")
	}

	nums := make([]int, 0, len(starts))
	for n := range starts {
		nums = append(nums, n)
	}
	sort.Ints(nums)

	chapters := make([]Chapter, len(nums))
	for i, n := range nums {
		chapters[i] = Chapter{ID: uint64(i + 1), Title: names[n], StartMs: starts[n]}
		if i > 0 {
			if starts[n] < chapters[i-1].StartMs {
				return nil, fmt.Errorf("CHAPTER%02d starts at %dms, before CHAPTER%02d - chapter times must not decrease", n, starts[n], nums[i-1])
			}
			chapters[i-1].EndMs = starts[n]
		}
	}
	return chapters, nil
}

// FormatOGMChapters renders chapters in the OGM simple format (sorted by
// start time, numbered from 01). Untitled chapters get an empty NAME line,
// per the de-facto convention.
func FormatOGMChapters(w io.Writer, chapters []Chapter) error {
	sorted := append([]Chapter(nil), chapters...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].StartMs < sorted[j].StartMs })
	for i, ch := range sorted {
		ms := ch.StartMs
		if _, err := fmt.Fprintf(w, "CHAPTER%02d=%02d:%02d:%02d.%03d\nCHAPTER%02dNAME=%s\n",
			i+1, ms/3_600_000, ms/60_000%60, ms/1000%60, ms%1000, i+1, ch.Title); err != nil {
			return err
		}
	}
	return nil
}

// ParseClockTime parses an HH:MM:SS.fraction clock time to milliseconds - the
// form OGM chapter files use, and the one the statistics DURATION tag that
// mainstream muxers (mkvgo included) write per track is spelled in ("00:48:10.680000000").
// Fraction digits past the millisecond are dropped, not rounded. An
// unparseable or out-of-range field is an error, never a guess.
func ParseClockTime(s string) (int64, error) { return parseOGMTime(s) }

// parseOGMTime parses HH:MM:SS.mmm (fraction optional). The fraction is
// parsed as decimal digits, not a float, so no millisecond is lost to
// floating-point rounding.
func parseOGMTime(s string) (int64, error) {
	bad := func() (int64, error) { return 0, fmt.Errorf("bad time %q, expected HH:MM:SS.mmm", s) }
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return bad()
	}
	secStr, fracStr, _ := strings.Cut(parts[2], ".")
	h, err1 := strconv.ParseInt(parts[0], 10, 64)
	m, err2 := strconv.ParseInt(parts[1], 10, 64)
	sec, err3 := strconv.ParseInt(secStr, 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || h < 0 || m < 0 || m > 59 || sec < 0 || sec > 59 {
		return bad()
	}
	var frac int64
	if fracStr != "" {
		for len(fracStr) < 3 {
			fracStr += "0"
		}
		var err error
		if frac, err = strconv.ParseInt(fracStr[:3], 10, 64); err != nil || frac < 0 {
			return bad()
		}
	}
	// The format puts no ceiling on HH, so the hours alone can be any int64
	// the field spells. Left to itself the multiply wraps and hands back a
	// negative timestamp, which reads as a chapter before the start of the
	// file rather than as the bad input it is. rest is under an hour, m, sec
	// and frac all being bounded above.
	rest := m*60_000 + sec*1000 + frac
	if h > math.MaxInt64/3_600_000 {
		return bad()
	}
	ms := h * 3_600_000
	if ms > math.MaxInt64-rest {
		return bad()
	}
	return ms + rest, nil
}
