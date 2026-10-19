package mp4

import (
	"cmp"
	"encoding/binary"
	"io"
	"slices"

	"github.com/gravity-zero/mkvgo/mkv"
)

// chapters.go - MP4 chapter markers, written as a Nero "chpl" box inside
// moov/udta. This is the most widely read chapter format on desktop players
// (most desktop players and probers). It carries only a start time and a title per
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
// untitled entries are ignored - chpl is a flat list).
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
// In addition to chpl (read by most desktop players), Apple players require a separate
// timed-text track whose samples are the chapter titles, linked from the media
// tracks by a tref/chap reference. The sample entry is the QuickTime 'text'
// type (not tx3g), and the media header is 'gmhd'. This mirrors what the de-facto muxer's
// mov muxer emits.

// buildChapterTextEntry builds the QuickTime 'text' sample entry used by a
// chapter track. The layout (and the trailing ftab font table) follow the de-facto muxer's
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
			ID:      uint64(i + 1), // ChapterUID must be non-zero for some readers
			StartMs: int64(start100ns / 10000),
			Title:   string(payload[pos : pos+titleLen]),
		})
		pos += titleLen
	}
	// A chapter ends where the next one in time starts, whatever order the file lists them in.
	slices.SortStableFunc(out, func(a, b mkv.Chapter) int { return cmp.Compare(a.StartMs, b.StartMs) })
	for i := 0; i+1 < len(out); i++ {
		out[i].EndMs = out[i+1].StartMs
	}
	return out
}

// buildChpl encodes the Nero chapter list. Start times are in 100-nanosecond
// units. The layout matches the de-facto mov muxer format:
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

// chapterTrackSamples returns the samples of a QuickTime chapter trak (titles and start times), nil when its tables do not read.
func chapterTrackSamples(trak []byte, fileSize int64) []inSample {
	tb, err := iterBoxes(trak)
	if err != nil {
		return nil
	}
	mdia, ok := findMemBox(tb, "mdia")
	if !ok {
		return nil
	}
	mb, err := iterBoxes(mdia.payload)
	if err != nil {
		return nil
	}
	var tr inTrack
	if mdhd, ok := findMemBox(mb, "mdhd"); ok {
		tr.timescale, _ = parseMdhd(mdhd.payload)
	}
	minf, ok := findMemBox(mb, "minf")
	if !ok || tr.timescale == 0 {
		return nil
	}
	nb, err := iterBoxes(minf.payload)
	if err != nil {
		return nil
	}
	stbl, ok := findMemBox(nb, "stbl")
	if !ok {
		return nil
	}
	sb, err := iterBoxes(stbl.payload)
	if err != nil || headerFrameCount(sb) > maxChapters || buildSampleTable(&tr, sb, fileSize) != nil {
		return nil
	}
	return tr.samples
}

// maxChapterSampleBytes is the largest chapter-track sample read: a title and its tx3g modifiers take bytes, not
// megabytes, so a larger declared size is not a title and is never allocated.
const maxChapterSampleBytes = 64 << 10

// readChapterTrack reads the chapter titles of a QuickTime chapter track, one ranged read per run of contiguous samples.
func readChapterTrack(r io.ReadSeeker, samples []inSample) []mkv.Chapter {
	for _, s := range samples {
		if s.size > maxChapterSampleBytes {
			return nil
		}
	}
	var chapters []mkv.Chapter
	for i := 0; i < len(samples); {
		j, end := i+1, samples[i].offset+int64(samples[i].size)
		for j < len(samples) && samples[j].offset == end && end-samples[i].offset < 1<<20 {
			end += int64(samples[j].size)
			j++
		}
		buf := make([]byte, end-samples[i].offset)
		if _, err := r.Seek(samples[i].offset, io.SeekStart); err != nil {
			return nil
		}
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil
		}
		for k := i; k < j; k++ {
			d := buf[samples[k].offset-samples[i].offset:][:samples[k].size]
			title, _ := decodeTx3g(d)
			chapters = append(chapters, mkv.Chapter{ID: uint64(len(chapters) + 1), Title: string(title), StartMs: samples[k].ctsMs, EndMs: samples[k].ctsMs + samples[k].durMs})
		}
		i = j
	}
	return chapters
}
