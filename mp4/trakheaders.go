package mp4

import (
	"bytes"
	"encoding/binary"
	"strconv"

	"github.com/gravity-zero/mkvgo/mkv"
)

// headerDurationSet reports whether a header's duration states one: 0 and all ones (0xFFFFFFFF, recorders still writing) mean unset.
func headerDurationSet(ticks uint64) bool {
	return ticks > 0 && ticks != 0xFFFFFFFF && ticks < 1<<62
}

// parseMovieHeader reads the movie timescale and duration from an mvhd box.
func parseMovieHeader(payload []byte) (timescale uint32, durationTicks uint64) {
	if len(payload) < 4 {
		return 0, 0
	}
	if payload[0] == 1 {
		// version1: creation(8) modification(8) timescale(4)@20 duration(8)@24
		if len(payload) >= 32 {
			timescale = binary.BigEndian.Uint32(payload[20:24])
			durationTicks = binary.BigEndian.Uint64(payload[24:32])
		}
		return timescale, durationTicks
	}
	// version0: creation(4) modification(4) timescale(4)@12 duration(4)@16
	if len(payload) >= 20 {
		timescale = binary.BigEndian.Uint32(payload[12:16])
		durationTicks = uint64(binary.BigEndian.Uint32(payload[16:20]))
	}
	return timescale, durationTicks
}

// parseMdhd reads an mdhd box and returns the media timescale and the ISO 639-2
// language code (decoded from the packed 16-bit field). lang is "" when the box
// is too short, the field is zero, or it decodes to "und" (undefined).
func parseMdhd(payload []byte) (timescale uint32, lang string) {
	if len(payload) < 4 {
		return 0, ""
	}
	// version0: creation(4)+modification(4)+timescale(4)+duration(4)+language(2)
	// version1: creation(8)+modification(8)+timescale(4)+duration(8)+language(2)
	tsOff, langOff := 12, 20
	if payload[0] == 1 {
		tsOff, langOff = 20, 32
	}
	if len(payload) >= tsOff+4 {
		timescale = binary.BigEndian.Uint32(payload[tsOff : tsOff+4])
	}
	if len(payload) >= langOff+2 {
		lang = decodeMdhdLanguage(binary.BigEndian.Uint16(payload[langOff : langOff+2]))
	}
	return timescale, lang
}

// mdhdDurationMs returns the track's duration in milliseconds from an mdhd box
// (duration ÷ media timescale), or 0 when absent/unset. Read head-only - the
// per-track counterpart of the movie duration in mvhd.
func mdhdDurationMs(payload []byte) int64 {
	if len(payload) < 4 {
		return 0
	}
	tsOff, durOff, durSize := 12, 16, 4
	if payload[0] == 1 {
		tsOff, durOff, durSize = 20, 24, 8
	}
	if len(payload) < tsOff+4 || len(payload) < durOff+durSize {
		return 0
	}
	ts := binary.BigEndian.Uint32(payload[tsOff : tsOff+4])
	if ts == 0 {
		return 0
	}
	var ticks uint64
	if durSize == 8 {
		ticks = binary.BigEndian.Uint64(payload[durOff : durOff+8])
	} else {
		ticks = uint64(binary.BigEndian.Uint32(payload[durOff : durOff+4]))
	}
	if !headerDurationSet(ticks) {
		return 0
	}
	return headerTicksToMs(ticks, ts)
}

// parseElst reads an edit list box and returns the first non-empty edit's
// media_time (media timescale) together with the total duration of any leading
// empty edits (movie timescale). ok is false when the box carries no edit that
// shifts the presentation timeline. Later edits (multi-segment timelines) are not
// modelled - only the leading delay and the start trim, which is what shifts the
// keyframe times.
func parseElst(payload []byte) (mediaTime, emptyDuration int64, ok bool) {
	if len(payload) < 8 {
		return 0, 0, false
	}
	version := payload[0]
	count := binary.BigEndian.Uint32(payload[4:8])
	off := 8
	for i := uint32(0); i < count; i++ {
		var segDur, mt int64
		if version == 1 {
			if off+20 > len(payload) {
				break
			}
			segDur = int64(binary.BigEndian.Uint64(payload[off : off+8]))
			mt = int64(binary.BigEndian.Uint64(payload[off+8 : off+16])) // signed
			off += 20
		} else {
			if off+12 > len(payload) {
				break
			}
			segDur = int64(binary.BigEndian.Uint32(payload[off : off+4]))
			mt = int64(int32(binary.BigEndian.Uint32(payload[off+4 : off+8]))) // signed
			off += 12
		}
		if mt < 0 { // empty edit → presentation delay
			emptyDuration += segDur
			continue
		}
		return mt, emptyDuration, true // first non-empty edit: the start trim
	}
	if emptyDuration > 0 {
		return 0, emptyDuration, true
	}
	return 0, 0, false
}

// decodeMdhdLanguage unpacks the mdhd language field. ISO 639-2/T is packed as
// three 5-bit values, each offset by 0x60, forming a lowercase code. QuickTime-
// origin files instead store a small Macintosh language code: the two forms are
// unambiguous because the smallest valid packed code ("aaa") is 0x421, so any
// value below 0x400 is a Mac code. Returns "" for an unknown/zero/"und" value and
// for anything that is not three a-z letters.
func decodeMdhdLanguage(packed uint16) string {
	if packed < 0x400 {
		return macLanguageToISO(packed)
	}
	c := [3]byte{
		byte((packed>>10)&0x1f) + 0x60,
		byte((packed>>5)&0x1f) + 0x60,
		byte(packed&0x1f) + 0x60,
	}
	for _, ch := range c {
		if ch < 'a' || ch > 'z' {
			return ""
		}
	}
	if s := string(c[:]); s != "und" {
		return s
	}
	return ""
}

// macLanguageCodes maps the Macintosh language codes (QuickTime mdhd) to ISO
// 639-2/T, the same vocabulary the packed form yields. It is deliberately limited
// to codes seen in real media; an unlisted code resolves to "" (treated as absent)
// rather than risk a wrong mapping.
var macLanguageCodes = map[uint16]string{
	0: "eng", 1: "fra", 2: "deu", 3: "ita", 4: "nld", 5: "swe", 6: "spa",
	7: "dan", 8: "por", 9: "nor", 10: "heb", 11: "jpn", 12: "ara", 13: "fin",
	14: "ell", 15: "isl", 16: "mlt", 17: "tur", 18: "hrv", 19: "zho", 20: "urd",
	21: "hin", 22: "tha", 23: "kor", 24: "lit", 25: "pol", 26: "hun", 27: "est",
	28: "lav", 30: "fao", 31: "fas", 32: "rus", 33: "zho", 35: "gle", 36: "sqi",
	37: "ron", 38: "ces", 39: "slk", 40: "slv", 41: "yid", 42: "srp", 43: "mkd",
	44: "bul", 45: "ukr", 46: "bel", 48: "kaz", 49: "aze", 51: "hye", 52: "kat",
	80: "vie", 81: "ind",
}

func macLanguageToISO(code uint16) string { return macLanguageCodes[code] }

// tkhdEnabled reports whether a track header's track_enabled flag (bit 0 of the
// 24-bit flags) is set. probers map this to the "default" stream disposition.
func tkhdEnabled(payload []byte) bool {
	if len(payload) < 4 {
		return false
	}
	flags := uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
	return flags&0x000001 != 0
}

// tkhdTrackID reads the track_ID from a track header. Its offset depends on the
// box version (v0 uses 32-bit times, v1 64-bit). Returns 0 when the box is short.
func tkhdTrackID(payload []byte) uint32 {
	off := 12 // v0: version+flags(4) + creation(4) + modification(4)
	if len(payload) > 0 && payload[0] == 1 {
		off = 20 // v1: version+flags(4) + creation(8) + modification(8)
	}
	if len(payload) < off+4 {
		return 0
	}
	return binary.BigEndian.Uint32(payload[off : off+4])
}

// tkhdOrientation reads the display matrix of a track header (16.16 fixed point, after the version-dependent timing fields) as a clockwise rotation and a horizontal mirror; none on an identity matrix or a short box.
func tkhdOrientation(payload []byte) (int, bool) {
	if len(payload) < 1 {
		return 0, false
	}
	matOff := 40 // v0: 4 + (4+4+4+4+4) + 8 + (2+2+2+2)
	if payload[0] == 1 {
		matOff = 52 // v1: timing fields are 8-byte
	}
	if len(payload) < matOff+8 {
		return 0, false
	}
	at := func(i int) float64 {
		if len(payload) < matOff+4*i+4 {
			return 0
		}
		return float64(int32(binary.BigEndian.Uint32(payload[matOff+4*i:]))) / 0x10000
	}
	return mkv.OrientationFromMatrix(at(0), at(1), at(3), at(4))
}

// quoteFourcc renders a four-character box type for a reason string, escaping any
// non-printable bytes (cover-art entries sometimes carry an all-zero fourcc).
func quoteFourcc(typ string) string { return strconv.Quote(typ) }

// parseElng reads the null-terminated BCP-47 tag from an elng (Extended Language
// Tag) box, skipping the 4-byte fullbox version/flags header.
func parseElng(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	s := payload[4:]
	if i := bytes.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return string(s)
}

// parseKind reads a kind box (ISO/IEC 14496-12): a fullbox holding a
// null-terminated schemeURI followed by a null-terminated value.
func parseKind(payload []byte) (scheme, value string) {
	if len(payload) < 4 {
		return "", ""
	}
	s := payload[4:] // skip version/flags
	i := bytes.IndexByte(s, 0)
	if i < 0 {
		return string(s), ""
	}
	scheme = string(s[:i])
	rest := s[i+1:]
	if j := bytes.IndexByte(rest, 0); j >= 0 {
		rest = rest[:j]
	}
	return scheme, string(rest)
}
