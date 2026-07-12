package mp4

import (
	"encoding/binary"
	"fmt"
)

// box.go - low-level ISO base media file format (ISO/IEC 14496-12) box encoding.
//
// This file knows nothing about Matroska or EBML. It only assembles big-endian
// boxes. Boxes are built into byte slices and nested by value, which keeps the
// call sites readable (see moov assembly in mux.go) at the cost of holding the
// moov tree in memory - acceptable because the moov is bounded by the sample
// count, and the large media payload (mdat) is streamed separately, never built
// as a box value here.

// bw is a tiny big-endian byte builder. Writes to its backing slice never fail
// (it only ever appends), so unlike mkv/writer's ew it carries no error: a box
// payload is always fully formed in memory before framing.
type bw struct {
	b []byte
}

func (w *bw) u8(v uint8)      { w.b = append(w.b, v) }
func (w *bw) u16(v uint16)    { w.b = binary.BigEndian.AppendUint16(w.b, v) }
func (w *bw) u32(v uint32)    { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *bw) u64(v uint64)    { w.b = binary.BigEndian.AppendUint64(w.b, v) }
func (w *bw) i8(v int8)       { w.b = append(w.b, byte(v)) }
func (w *bw) i16(v int16)     { w.u16(uint16(v)) }
func (w *bw) i32(v int32)     { w.u32(uint32(v)) }
func (w *bw) bytes(p []byte)  { w.b = append(w.b, p...) }
func (w *bw) zeros(n int)     { w.b = append(w.b, make([]byte, n)...) }
func (w *bw) fourcc(s string) { w.b = append(w.b, s[0], s[1], s[2], s[3]) }

// u24 writes the low 24 bits of v (used for fullbox flags).
func (w *bw) u24(v uint32) { w.b = append(w.b, byte(v>>16), byte(v>>8), byte(v)) }

// box frames payload as a complete box: size(4) + type(4) + payload. When the
// total would not fit in 32 bits it uses the 64-bit largesize form
// (size=1, type, largesize(8), payload) as the spec requires.
func box(typ string, payload []byte) []byte {
	const hdr32 = 8
	total := uint64(hdr32 + len(payload))
	if total <= 0xFFFFFFFF {
		var w bw
		w.u32(uint32(total))
		w.fourcc(typ)
		w.bytes(payload)
		return w.b
	}
	var w bw
	w.u32(1) // size==1 signals 64-bit largesize follows the type
	w.fourcc(typ)
	w.u64(total + 8) // +8 for the largesize field itself
	w.bytes(payload)
	return w.b
}

// boxf builds a box whose payload is produced by fn writing into a fresh bw.
func boxf(typ string, fn func(*bw)) []byte {
	var w bw
	fn(&w)
	return box(typ, w.b)
}

// fullBox builds a box prefixed with the FullBox header: version(1) + flags(24).
func fullBox(typ string, version uint8, flags uint32, fn func(*bw)) []byte {
	return boxf(typ, func(w *bw) {
		w.u8(version)
		w.u24(flags)
		fn(w)
	})
}

// container builds a box whose payload is the concatenation of child boxes.
func container(typ string, children ...[]byte) []byte {
	var w bw
	for _, c := range children {
		w.bytes(c)
	}
	return box(typ, w.b)
}

// fixed16_16 converts an integer to 16.16 fixed-point (e.g. width/height, rate).
func fixed16_16(v uint32) uint32 { return v << 16 }

// unityMatrix is the ISO-BMFF identity transformation matrix (a=d=1.0 in 16.16,
// w=1.0 in 2.30). Written into mvhd and tkhd.
var unityMatrix = [9]uint32{
	0x00010000, 0, 0,
	0, 0x00010000, 0,
	0, 0, 0x40000000,
}

func (w *bw) matrix(m [9]uint32) {
	for _, v := range m {
		w.u32(v)
	}
}

// descLen appends an MPEG-4 descriptor length (ISO/IEC 14496-1 expandable size)
// to the builder. Values are encoded 7 bits per byte, big-endian, with the high
// bit set on every byte except the last. Used by esds.
func (w *bw) descLen(n int) {
	if n < 0 {
		n = 0
	}
	var tmp [4]byte
	i := len(tmp)
	for {
		i--
		tmp[i] = byte(n & 0x7F)
		n >>= 7
		if n == 0 {
			break
		}
	}
	for j := i; j < len(tmp); j++ {
		b := tmp[j]
		if j != len(tmp)-1 {
			b |= 0x80
		}
		w.u8(b)
	}
}

// descriptor builds an MPEG-4 descriptor: tag(1) + expandable-length + payload.
func descriptor(tag uint8, payload []byte) []byte {
	var w bw
	w.u8(tag)
	w.descLen(len(payload))
	w.bytes(payload)
	return w.b
}

// packLanguage encodes a 3-letter ISO-639-2 code as the 15-bit packed form used
// by mdhd: three 5-bit values, each (char - 0x60). Non-conforming input yields
// "und" (undetermined), the spec's fallback.
func packLanguage(lang string) uint16 {
	if len(lang) != 3 || !isLowerAlpha(lang) {
		lang = "und"
	}
	return uint16(lang[0]-0x60)<<10 | uint16(lang[1]-0x60)<<5 | uint16(lang[2]-0x60)
}

func isLowerAlpha(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

// errf is a small helper to keep error construction terse and consistent.
func errf(format string, a ...any) error { return fmt.Errorf("mp4: "+format, a...) }
