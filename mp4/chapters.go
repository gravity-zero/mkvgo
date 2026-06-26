package mp4

import (
	"encoding/binary"

	"github.com/gravity-zero/mkvgo/mkv"
)

// chapters.go — MP4 chapter markers, written as a Nero "chpl" box inside
// moov/udta. This is the most widely read chapter format on desktop players
// (VLC, mpv, MediaInfo, ffprobe). It carries only a start time and a title per
// chapter, which is exactly what Matroska's ChapterAtom provides.

// maxChapters is the chpl count field width (one byte).
const maxChapters = 255

// buildChplBox returns the chapter-list (chpl) box, or nil when there are no titled
// top-level chapters. The caller places it in the moov udta (shared with the title).
func buildChplBox(chapters []mkv.Chapter) []byte {
	chapters = flattenChapters(chapters)
	if len(chapters) == 0 {
		return nil
	}
	return buildChpl(chapters)
}

// flattenChapters returns the top-level chapters with a title (sub-chapters and
// untitled entries are ignored — chpl is a flat list).
func flattenChapters(chapters []mkv.Chapter) []mkv.Chapter {
	out := chapters[:0:0]
	for _, c := range chapters {
		if c.Title != "" {
			out = append(out, c)
		}
	}
	return out
}

// --- QuickTime chapter track (for Apple players) -----------------------------
//
// In addition to chpl (read by VLC/mpv/ffmpeg), Apple players require a separate
// timed-text track whose samples are the chapter titles, linked from the media
// tracks by a tref/chap reference. The sample entry is the QuickTime 'text'
// type (not tx3g), and the media header is 'gmhd'. This mirrors what ffmpeg's
// mov muxer emits.

// buildChapterTextEntry builds the QuickTime 'text' sample entry used by a
// chapter track. The layout (and the trailing ftab font table) follow ffmpeg's
// output; it carries no per-chapter data.
func buildChapterTextEntry() []byte {
	ftab := boxf("ftab", func(w *bw) {
		w.u16(1) // entry count
		w.u16(1) // font ID
		w.u8(0)  // empty font name
	})
	return boxf("text", func(w *bw) {
		w.zeros(6) // SampleEntry reserved
		w.u16(1)   // data_reference_index
		w.u32(1)   // displayFlags
		w.u32(0)   // textJustification
		w.zeros(6) // background-color (RGB16)
		w.u16(0)   // defaultTextBox: top
		w.u16(0)   // left
		w.u16(0)   // bottom
		w.u16(1)   // right
		w.zeros(8) // reserved (scrpStartChar / height / ascent)
		w.bytes(ftab)
	})
}

// buildGmhd builds the base media header ('gmhd') for a QuickTime text track.
func buildGmhd() []byte {
	gmin := boxf("gmin", func(w *bw) {
		w.u32(0)      // version/flags
		w.u16(0x0040) // graphicsmode
		w.u16(0x8000) // opcolor R
		w.u16(0x8000) // opcolor G
		w.u16(0x8000) // opcolor B
		w.u16(0)      // balance
		w.u16(0)      // reserved
	})
	text := boxf("text", func(w *bw) { w.matrix(unityMatrix) })
	return container("gmhd", gmin, text)
}

// encodeChapterSample formats a chapter title as a QuickTime text sample:
// a 16-bit length, the UTF-8 title, then an 'encd' text-encoding atom.
func encodeChapterSample(title string) []byte {
	b := []byte(title)
	if len(b) > 0xFFFF {
		b = b[:truncateRunes(b, 0xFFFF)]
	}
	encd := boxf("encd", func(w *bw) { w.u32(0x00000100) })
	out := make([]byte, 0, 2+len(b)+len(encd))
	out = append(out, byte(len(b)>>8), byte(len(b)))
	out = append(out, b...)
	out = append(out, encd...)
	return out
}

// buildTrefChap builds a tref box referencing the chapter track by ID.
func buildTrefChap(chapterTrackID uint32) []byte {
	chap := boxf("chap", func(w *bw) { w.u32(chapterTrackID) })
	return container("tref", chap)
}

// parseChpl decodes a Nero chpl box payload into chapters. It is defensive
// against malformed input (truncated entries stop the parse). End times are
// derived from the following chapter's start, the last left open (0).
func parseChpl(payload []byte) []mkv.Chapter {
	if len(payload) < 9 {
		return nil
	}
	count := int(payload[8])
	out := make([]mkv.Chapter, 0, count)
	pos := 9
	for i := 0; i < count; i++ {
		if pos+9 > len(payload) {
			break
		}
		start100ns := binary.BigEndian.Uint64(payload[pos : pos+8])
		titleLen := int(payload[pos+8])
		pos += 9
		if pos+titleLen > len(payload) {
			break
		}
		out = append(out, mkv.Chapter{
			ID:      uint64(i + 1), // ChapterUID must be non-zero for some readers (ffmpeg)
			StartMs: int64(start100ns / 10000),
			Title:   string(payload[pos : pos+titleLen]),
		})
		pos += titleLen
	}
	for i := 0; i+1 < len(out); i++ {
		out[i].EndMs = out[i+1].StartMs
	}
	return out
}

// buildChpl encodes the Nero chapter list. Start times are in 100-nanosecond
// units. The layout matches the de-facto format written by ffmpeg's mov muxer:
// version(1)+flags(3), 4 reserved bytes, a 1-byte count, then per chapter an
// 8-byte start, a 1-byte title length and the UTF-8 title.
func buildChpl(chapters []mkv.Chapter) []byte {
	n := len(chapters)
	if n > maxChapters {
		n = maxChapters
	}
	return boxf("chpl", func(w *bw) {
		w.u32(0x01000000) // version 1, flags 0
		w.u32(0)          // reserved
		w.u8(uint8(n))
		for i := 0; i < n; i++ {
			start := chapters[i].StartMs
			if start < 0 {
				start = 0
			}
			w.u64(uint64(start) * 10000) // ms → 100ns units
			title := []byte(chapters[i].Title)
			if len(title) > 255 {
				title = title[:truncateRunes(title, 255)]
			}
			w.u8(uint8(len(title)))
			w.bytes(title)
		}
	})
}
