// Package mpegts recognises an MPEG transport stream (or a Blu-ray M2TS) from its first packets, so every reader
// refuses one by name, whatever the file is called: mkvgo does not read MPEG-TS.
package mpegts

import "io"

// layouts are where the sync byte 0x47 sits: every 188 bytes (TS), or every 192 after a 4-byte timecode (M2TS).
var layouts = []struct{ start, stride int64 }{{0, 188}, {4, 192}}

// PrefixLen is how many leading bytes Prefix needs to see three packets of either layout.
const PrefixLen = 4 + 2*192 + 1

// At reports whether r opens on three MPEG-TS packets in a row; it reads one byte per packet. size < 0 is unknown.
func At(r io.ReadSeeker, size int64) bool {
	at := func(off int64) bool {
		var b [1]byte
		if size >= 0 && off >= size {
			return false
		}
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			return false
		}
		_, err := io.ReadFull(r, b[:])
		return err == nil && b[0] == 0x47
	}
	for _, l := range layouts {
		if at(l.start) && at(l.start+l.stride) && at(l.start+2*l.stride) {
			return true
		}
	}
	return false
}

// Prefix reports whether b, a source's first bytes, opens on three MPEG-TS packets in a row.
func Prefix(b []byte) bool {
	for _, l := range layouts {
		if end := l.start + 2*l.stride; end < int64(len(b)) && b[l.start] == 0x47 && b[l.start+l.stride] == 0x47 && b[end] == 0x47 {
			return true
		}
	}
	return false
}
