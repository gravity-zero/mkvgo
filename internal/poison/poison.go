// Package poison makes a recycled buffer fail loudly under test: built with
// the mkvgo_poison tag, a buffer handed back for reuse is filled with 0xDB and
// a fresh one takes its place, so code still holding the old one reads garbage
// instead of whatever bytes the next use happened to leave there.
package poison

// Byte is what a recycled buffer is filled with.
const Byte = 0xDB

// Fill overwrites b with Byte when poisoning is on.
func Fill(b []byte) {
	if On {
		for i := range b {
			b[i] = Byte
		}
	}
}

// Reuse returns buf[:size], grown when too small; poisoning on, buf is filled with Byte and a fresh slice returned.
func Reuse(buf []byte, size int) []byte {
	if On {
		Fill(buf[:cap(buf)])
		return make([]byte, size)
	}
	if cap(buf) < size {
		return make([]byte, size)
	}
	return buf[:size]
}
