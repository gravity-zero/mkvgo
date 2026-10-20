package subtitle

import (
	"strings"
	"unicode"
)

// Reading-time bounds of a cue that states no end: 17 characters a second, held at least one second, at most seven.
const (
	readingCharsPerSec = 17
	minReadingMs       = 1000
	maxReadingMs       = 7000
)

// ReadingMs is how long a cue's text takes to read: its visible characters (ASS overrides, markup tags and line
// breaks left out, a '<' that no '>' closes read as text) at 17 a second, between one and seven seconds.
func ReadingMs(text string) int64 {
	n := int64(0)
	depth, tag := 0, false
	text = strings.ReplaceAll(strings.ReplaceAll(text, `\N`, " "), `\n`, " ")
	last := strings.LastIndexByte(text, '>') // a '<' opens a tag only when a '>' follows it
	for i, r := range text {
		switch {
		case r == '{':
			depth++
		case r == '}' && depth > 0:
			depth--
		case r == '<' && !tag && i < last:
			tag = true
		case r == '>' && tag:
			tag = false
		case depth == 0 && !tag && r != '\n' && r != '\r' && unicode.IsPrint(r):
			n++
		}
	}
	return min(max(n*1000/readingCharsPerSec, minReadingMs), maxReadingMs)
}

// CueEndMs is where a cue that states no end stops: once its text is read (ReadingMs), at the next cue that starts
// after it (nextMs, when hasNext) or at the media's end (mediaEndMs, 0 when unknown), whichever comes first.
func CueEndMs(startMs int64, text string, nextMs int64, hasNext bool, mediaEndMs int64) int64 {
	end := startMs + ReadingMs(text)
	if hasNext && nextMs > startMs && nextMs < end {
		end = nextMs
	}
	if mediaEndMs > startMs && mediaEndMs < end {
		end = mediaEndMs
	}
	return end
}

// SettleCueEnds ends every cue whose end is unset (at or before its start) as CueEndMs does, the next cue being the
// first that starts after it. Cues must be in start order; mediaEndMs is 0 when the media's end is unknown.
func SettleCueEnds(cues []Cue, mediaEndMs int64) {
	// later holds, nearest last, the cues after i each starting after every cue between it and i: the first one
	// starting after cue i is found by dropping those that do not, once for all cues.
	var later []int
	for i := len(cues) - 1; i >= 0; i-- {
		for len(later) > 0 && cues[later[len(later)-1]].StartMs <= cues[i].StartMs {
			later = later[:len(later)-1]
		}
		if cues[i].EndMs <= cues[i].StartMs {
			next, ok := int64(0), len(later) > 0
			if ok {
				next = cues[later[len(later)-1]].StartMs
			}
			cues[i].EndMs = CueEndMs(cues[i].StartMs, cues[i].Text, next, ok, mediaEndMs)
		}
		later = append(later, i)
	}
}
