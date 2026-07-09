package mp4

// chaptermarkers.go - Options.ChapterMarkers: opt-in chapter/ad-insertion
// markers riding on the HLS media playlist (EXT-X-DATERANGE) and the DASH
// manifest (a Period-level EventStream), built from the source's Matroska/
// MP4 chapters (mkv.Chapter, already threaded into the packaging source by
// openPackagingSource for both container kinds). Off by default: nothing
// here runs unless the option is set, and the media segments are never
// touched - only the playlist/manifest TEXT gains lines.

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

// chapterMarkers resolves the source's chapters into the flat, titled,
// end-time-resolved sequence the HLS and DASH writers share. totalMs is the
// presentation's total duration (fragTrack.presentMs of the primary/video
// track - computed identically by the full pass and the on-demand plan), used
// as the last chapter's end when the source leaves it open (the common case:
// muxers write only ChapterTimeStart). Returns nil when the option is off or
// the source has no titled top-level chapters, so both writers stay a no-op -
// the gate the OFF invariant and the "no chapters" test rely on.
func chapterMarkers(o *Options, chapters []mkv.Chapter, totalMs int64) []mkv.Chapter {
	if o == nil || !o.ChapterMarkers {
		return nil
	}
	chs := flattenChapters(chapters)
	if len(chs) == 0 {
		return nil
	}
	out := make([]mkv.Chapter, len(chs))
	copy(out, chs)
	for i := range out {
		switch {
		case out[i].EndMs > out[i].StartMs:
			// An explicit ChapterTimeEnd was already set - keep it.
		case i+1 < len(out):
			out[i].EndMs = out[i+1].StartMs
		default:
			out[i].EndMs = totalMs
		}
		if out[i].EndMs < out[i].StartMs {
			out[i].EndMs = out[i].StartMs // never a negative duration
		}
	}
	return out
}

// chapterDateEpoch is the fixed zero epoch EXT-X-DATERANGE's START-DATE is
// computed from (epoch + chapter start offset), so the same source always
// renders the same dates - no wall-clock dependency, reproducible output.
var chapterDateEpoch = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// buildChapterDateRanges renders one #EXT-X-DATERANGE per chapter, in
// playlist order, for the video media playlist: a unique ID, a START-DATE
// derived from chapterDateEpoch, the chapter's DURATION in seconds, and its
// title (X-CHAPTER-TITLE, quoted/escaped like every other quoted playlist
// attribute this package writes, e.g. EXT-X-MEDIA's NAME).
func buildChapterDateRanges(chapters []mkv.Chapter) []byte {
	var b []byte
	for i, ch := range chapters {
		start := chapterDateEpoch.Add(time.Duration(ch.StartMs) * time.Millisecond)
		dur := float64(ch.EndMs-ch.StartMs) / 1000
		b = append(b, fmt.Sprintf("#EXT-X-DATERANGE:ID=%q,START-DATE=%q,DURATION=%.3f,X-CHAPTER-TITLE=%q\n",
			fmt.Sprintf("chapter-%d", i+1), start.Format("2006-01-02T15:04:05.000Z"), dur, ch.Title)...)
	}
	return b
}

// chapterEventStreamScheme identifies mkvgo's DASH chapter-annotation Events.
// No chapter EventStream scheme is IANA-registered (ISO/IEC 23009-1 leaves
// schemeIdUri application-defined, section 5.10.2); this URI is this
// package's own, documented in docs/streaming.md.
const chapterEventStreamScheme = "urn:mkvgo:dash:chapter:2024"

// buildChapterEventStream renders the Period-level <EventStream> (millisecond
// timescale, matching Chapter.StartMs/EndMs directly) - one <Event> per
// chapter, its title as the event's text body, XML-escaped. Per ISO/IEC
// 23009-1 5.3.9, EventStream is a child of Period (not AdaptationSet), so it
// applies to the whole presentation timeline rather than one rendition.
func buildChapterEventStream(chapters []mkv.Chapter) string {
	var b strings.Builder
	fmt.Fprintf(&b, "    <EventStream schemeIdUri=%q timescale=\"1000\">\n", chapterEventStreamScheme)
	for i, ch := range chapters {
		fmt.Fprintf(&b, `      <Event id="%d" presentationTime="%d" duration="%d">%s</Event>`+"\n",
			i+1, ch.StartMs, ch.EndMs-ch.StartMs, xmlEscapeText(ch.Title))
	}
	b.WriteString("    </EventStream>\n")
	return b.String()
}

// xmlEscapeText XML-escapes s for use as element text content (titles may
// carry '&', '<', '>', quotes - none of which are valid raw inside an
// element body).
func xmlEscapeText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
