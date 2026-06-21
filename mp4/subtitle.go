package mp4

import (
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// subtitle.go — SRT (S_TEXT/UTF8) subtitles carried as MP4 timed text (tx3g,
// a.k.a. mov_text). Each Matroska subtitle cue becomes one tx3g sample whose
// payload is a 16-bit length followed by UTF-8 text; the gaps between cues are
// filled with empty samples so the text track is continuous, as players expect.

// defaultCueDurMs is used for the last cue when its BlockDuration is absent.
const defaultCueDurMs = 2000

// emptyCue is a zero-length tx3g sample (text length 0), used to fill the gaps
// between subtitle cues.
var emptyCue = []byte{0x00, 0x00}

// srtEntry builds a tx3g (TextSampleEntry) box. The styling mirrors what
// ffmpeg's mov_text muxer emits: bottom-centred, white text, transparent
// background, with a single-font table.
func srtEntry(t *mkv.Track, _ []byte) ([]byte, error) {
	const font = "Serif"
	ftab := boxf("ftab", func(w *bw) {
		w.u16(1) // entry count
		w.u16(1) // font ID
		w.u8(uint8(len(font)))
		w.bytes([]byte(font))
	})
	return boxf("tx3g", func(w *bw) {
		w.zeros(6)                              // SampleEntry reserved
		w.u16(1)                                // data_reference_index
		w.u32(0)                                // displayFlags
		w.u8(1)                                 // horizontal-justification: centre
		w.i8(-1)                                // vertical-justification: bottom
		w.bytes([]byte{0x00, 0x00, 0x00, 0x00}) // background-color-rgba (transparent)
		// BoxRecord default-text-box: top, left, bottom, right.
		w.u16(0)
		w.u16(0)
		w.u16(0)
		w.u16(0)
		// StyleRecord: startChar, endChar, font-ID, face, size, text-color-rgba.
		w.u16(0)
		w.u16(0)
		w.u16(1)
		w.u8(0)
		w.u8(18)
		w.bytes([]byte{0xFF, 0xFF, 0xFF, 0xFF}) // white, opaque
		w.bytes(ftab)
	}), nil
}

// encodeCue formats one cue's text as a tx3g sample: a 16-bit length prefix
// followed by the UTF-8 bytes. Inline markup (<i>, <b>…) is stripped since tx3g
// carries styling out of band. Text longer than 65535 bytes is truncated on a
// rune boundary.
func encodeCue(text []byte) []byte {
	s := stripMarkup(string(text))
	b := []byte(s)
	if len(b) > 0xFFFF {
		b = b[:truncateRunes(b, 0xFFFF)]
	}
	out := make([]byte, 0, 2+len(b))
	out = append(out, byte(len(b)>>8), byte(len(b)))
	out = append(out, b...)
	return out
}

// stripMarkup removes angle-bracket tags (e.g. SRT's <i>/<b>/<font>) so they do
// not appear as literal text. Text without '<' is returned unchanged.
func stripMarkup(s string) string {
	if !strings.ContainsRune(s, '<') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	depth := 0
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
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

// truncateRunes returns the largest length <= max that ends on a UTF-8 boundary.
func truncateRunes(b []byte, max int) int {
	if max > len(b) {
		max = len(b)
	}
	for max > 0 && b[max]&0xC0 == 0x80 { // mid-rune continuation byte
		max--
	}
	return max
}

// addTextBlock handles one subtitle cue using one-cue lookahead so a cue's
// duration can be bounded by the start of the next cue (and to fill gaps).
func (t *outTrack) addTextBlock(cw *countWriter, b mkv.Block) error {
	if t.hasPendingCue {
		if err := t.flushPendingCue(cw, b.Timecode); err != nil {
			return err
		}
	}
	t.hasPendingCue = true
	t.pendCuePTS = b.Timecode
	t.pendCueDur = b.Duration
	t.pendCueText = append(t.pendCueText[:0], b.Data...)
	return nil
}

// flushPendingCue emits the buffered cue (and any surrounding empty samples).
// nextStart is the next cue's PTS, or -1 at end of stream.
func (t *outTrack) flushPendingCue(cw *countWriter, nextStart int64) error {
	// Empty sample covering the lead-in before the first cue.
	if len(t.samples.samples) == 0 && t.pendCuePTS > 0 {
		if err := t.emitSample(cw, emptyCue, 0, t.pendCuePTS, true); err != nil {
			return err
		}
	}

	dur := t.pendCueDur
	if nextStart >= 0 && nextStart > t.pendCuePTS {
		if gap := nextStart - t.pendCuePTS; dur <= 0 || dur > gap {
			dur = gap // clamp to avoid overlap; also covers missing BlockDuration
		}
	}
	if dur <= 0 {
		dur = defaultCueDurMs
	}
	if err := t.emitSample(cw, encodeCue(t.pendCueText), t.pendCuePTS, dur, true); err != nil {
		return err
	}

	// Empty sample bridging the gap to the next cue.
	if end := t.pendCuePTS + dur; nextStart > end {
		if err := t.emitSample(cw, emptyCue, end, nextStart-end, true); err != nil {
			return err
		}
	}
	t.hasPendingCue = false
	return nil
}
