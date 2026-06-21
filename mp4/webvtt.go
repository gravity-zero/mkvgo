package mp4

import (
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// webvtt.go — native WebVTT carriage in MP4 (wvtt, ISO/IEC 14496-30) plus the
// text flatteners used when Options.FlattenStyledSubs reduces styled subtitles
// (WebVTT, ASS/SSA) to plain tx3g timed text.
//
// Unlike ASS, WebVTT has a standard, lossless MP4 representation, so by default
// a WebVTT track round-trips as wvtt rather than being flattened: the sample
// entry carries a WebVTTConfigurationBox (vttC) with the file header, and each
// cue is a VTTCueBox (vttc) wrapping a CuePayloadBox (payl).

// wvttEntry builds a WVTTSampleEntry: a PlainTextSampleEntry ('wvtt') carrying a
// vttC config box. The config is the WebVTT file header; when the track has a
// CodecPrivate (Matroska stores the header there) it is preserved verbatim,
// otherwise the minimal valid header "WEBVTT" is emitted.
func wvttEntry(t *mkv.Track, _ []byte) ([]byte, error) {
	config := "WEBVTT"
	if t != nil && len(t.CodecPrivate) > 0 {
		if cp := string(t.CodecPrivate); strings.HasPrefix(cp, "WEBVTT") {
			config = cp
		} else {
			config = "WEBVTT\n" + cp
		}
	}
	vttC := boxf("vttC", func(w *bw) { w.bytes([]byte(config)) })
	return boxf("wvtt", func(w *bw) {
		w.zeros(6) // SampleEntry reserved
		w.u16(1)   // data_reference_index
		w.bytes(vttC)
	}), nil
}

// encodeWVTTCue builds one wvtt sample: a single VTTCueBox (vttc) with a
// CuePayloadBox (payl) holding the cue text. Per-cue settings/identifiers that
// Matroska stores in BlockAdditions are not carried (the block reader does not
// surface them); the payload text and its inline markup are preserved verbatim.
func encodeWVTTCue(payload []byte) []byte {
	payl := boxf("payl", func(w *bw) { w.bytes(payload) })
	return boxf("vttc", func(w *bw) { w.bytes(payl) })
}

// wvttEmptyCue is a VTTEmptyCueBox (vtte): the sample filling the gaps between
// cues so the wvtt track is continuous, as players expect.
var wvttEmptyCue = boxf("vtte", func(_ *bw) {})

// decodeWVTT extracts the cue text from a wvtt sample by reading the payl box of
// each vttc. It returns ok=false for an empty (vtte-only) sample. Multiple cues
// in one sample are joined with newlines.
func decodeWVTT(data []byte) ([]byte, bool) {
	boxes, err := iterBoxes(data)
	if err != nil {
		return nil, false
	}
	var out []byte
	for _, b := range boxes {
		if b.typ != "vttc" {
			continue
		}
		children, err := iterBoxes(b.payload)
		if err != nil {
			continue
		}
		for _, c := range children {
			if c.typ == "payl" {
				if len(out) > 0 {
					out = append(out, '\n')
				}
				out = append(out, c.payload...)
			}
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// extractWVTTConfig reads the vttC (WebVTTConfigurationBox) from a wvtt sample
// entry and stores the WebVTT file header as the track's CodecPrivate, so the
// header round-trips back into Matroska. A missing vttC leaves CodecPrivate nil.
func extractWVTTConfig(tr *inTrack, payload []byte) {
	const plainTextHdr = 8 // 6 reserved + 2 data_reference_index
	if len(payload) < plainTextHdr {
		return
	}
	children, err := iterBoxes(payload[plainTextHdr:])
	if err != nil {
		return
	}
	for _, b := range children {
		if b.typ == "vttC" {
			tr.codecPrivate = append([]byte(nil), b.payload...)
			return
		}
	}
}

// decodeSubtitleSample turns one MP4 subtitle sample back into its Matroska cue
// payload, dispatching on the reconstructed codec. ok=false means the sample is
// an empty/gap filler with no Matroska block.
func decodeSubtitleSample(codec string, data []byte) ([]byte, bool) {
	if codec == "webvtt" {
		return decodeWVTT(data)
	}
	return decodeTx3g(data)
}

// flattenASS reduces a Matroska S_TEXT/ASS block to plain text: it drops the
// eight leading dialogue fields (ReadOrder, Layer, Style, Name, three margins,
// Effect), strips override blocks ({\...}), and turns the ASS escapes \N/\n into
// newlines and \h into a space. Drawing commands are not specially handled (their
// braces are stripped, leaving the coordinates as text — matching ffmpeg's
// mov_text). It is lossy by nature: all styling and positioning is discarded.
func flattenASS(payload []byte) []byte {
	s := string(payload)
	// S_TEXT/ASS framing: ReadOrder,Layer,Style,Name,MarginL,MarginR,MarginV,Effect,Text.
	if parts := strings.SplitN(s, ",", 9); len(parts) == 9 {
		s = parts[8]
	}
	s = stripASSTags(s)
	s = strings.ReplaceAll(s, `\N`, "\n")
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\h`, " ")
	return []byte(s)
}

// stripASSTags removes ASS override blocks delimited by { and }. Text without a
// '{' is returned unchanged.
func stripASSTags(s string) string {
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
