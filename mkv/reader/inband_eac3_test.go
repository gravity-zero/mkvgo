package reader

import (
	"bytes"
	"testing"
)

// eac3Frame is an E-AC-3 syncframe of size bytes (a multiple of 2): strmtyp,
// substreamid, acmod, lfeon, then, for a dependent substream, chanmap when
// not negative. The audio blocks are zeros: only the header is read.
func eac3Frame(strmtyp, substream, acmod, lfe uint32, chanmap int, size int) []byte {
	return eac3FrameBlocks(strmtyp, substream, acmod, lfe, chanmap, size, 3, nil)
}

// eac3FrameBlocks is eac3Frame with its numblkscod, then tail bytes after the
// header fields read (standing for what follows them: mixing metadata, the
// JOC object metadata of an Atmos stream).
func eac3FrameBlocks(strmtyp, substream, acmod, lfe uint32, chanmap int, size int, numblkscod uint32, tail []byte) []byte {
	var bw bitWriter
	bw.write(0x0B77, 16)
	bw.write(strmtyp, 2)
	bw.write(substream, 3)
	bw.write(uint32(size/2-1), 11) // frmsiz: 16-bit words minus one
	bw.write(0, 2)                 // fscod: 48 kHz
	bw.write(numblkscod, 2)        // numblkscod: 3 = 6 blocks
	bw.write(acmod, 3)
	bw.write(lfe, 1)
	bw.write(16, 5) // bsid
	bw.write(31, 5) // dialnorm
	bw.write(0, 1)  // compre
	if acmod == 0 {
		bw.write(31, 5) // dialnorm2
		bw.write(0, 1)  // compr2e
	}
	if strmtyp == 1 {
		bw.write(b2u(chanmap >= 0), 1) // chanmape
		if chanmap >= 0 {
			bw.write(uint32(chanmap), 16)
		}
	}
	f := append(bw.bytes(), tail...)
	return append(f, make([]byte, size-len(f))...)
}

// ac3CoreFrame is a 448 kbit/s 48 kHz AC-3 syncframe (1792 bytes) of acmod
// 3/2 with LFE: the 5.1 core some E-AC-3 tracks open their access units with.
func ac3CoreFrame() []byte {
	var bw bitWriter
	bw.write(0x0B77, 16)
	bw.write(0, 16) // crc1
	bw.write(0, 2)  // fscod: 48 kHz
	bw.write(30, 6) // frmsizecod: 448 kbit/s
	bw.write(6, 5)  // bsid
	bw.write(0, 3)  // bsmod
	bw.write(7, 3)  // acmod 3/2
	bw.write(0, 2)  // cmixlev
	bw.write(0, 2)  // surmixlev
	bw.write(1, 1)  // lfeon
	f := bw.bytes()
	return append(f, make([]byte, 1792-len(f))...)
}

func b2u(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// An E-AC-3 access unit counts the speakers of its independent substream - an
// E-AC-3 one, or an AC-3 core - and of the dependent substreams after it: a
// 5.1 core plus a dependent carrying the back pair (chanmap Lrs/Rrs) is 7.1.
func TestEAC3DependentSubstreamChannels(t *testing.T) {
	const backPair = 1 << (15 - 6) // chanmap location 6: Lrs/Rrs
	core := eac3Frame(0, 0, 7, 1, -1, 64)
	for _, c := range []struct {
		name  string
		frame []byte
		want  uint8
	}{
		{"5.1 alone", core, 6},
		{"7.1: 5.1 + back pair", append(append([]byte(nil), core...), eac3Frame(1, 0, 2, 0, backPair, 48)...), 8},
		{"7.1 then the next unit's core", append(append(append([]byte(nil), core...), eac3Frame(1, 0, 2, 0, backPair, 48)...), core...), 8},
		{"2.0 alone", eac3Frame(0, 0, 2, 0, -1, 32), 2},
		{"a second programme is not counted", append(append([]byte(nil), core...), eac3Frame(0, 1, 2, 0, -1, 32)...), 6},
		{"7.1 on an AC-3 core: 5.1 + back pair", append(ac3CoreFrame(), eac3Frame(1, 0, 2, 0, backPair, 48)...), 8},
		{"AC-3 alone", ac3CoreFrame(), 6},
		// The positions are joined, never added: a dependent naming the core's
		// surrounds again replaces them, as a decoder ORs the channel masks.
		{"a dependent repeating Ls/Rs: 8, not 10", append(append([]byte(nil), core...), eac3Frame(1, 0, 2, 0, 1<<(15-3)|1<<(15-4)|backPair, 48)...), 8},
		{"chanmape=0: the dependent's acmod and lfeon place it", append(append([]byte(nil), core...), eac3Frame(1, 0, 6, 1, -1, 48)...), 6},
		{"chanmape=0 adding a centre to a 2/2 core", append(eac3Frame(0, 0, 6, 0, -1, 64), eac3Frame(1, 0, 1, 0, -1, 48)...), 5},
		{"two programmes, each 7.1: programme 0 only", append(append(append(append([]byte(nil), core...), eac3Frame(1, 0, 2, 0, backPair, 48)...), eac3Frame(0, 1, 7, 1, -1, 64)...), eac3Frame(1, 1, 2, 0, backPair, 48)...), 8},
		{"numblkscod 0: one block a frame, the unit's frames in a row", append(append(append(eac3FrameBlocks(0, 0, 7, 1, -1, 64, 0, nil), eac3FrameBlocks(1, 0, 2, 0, backPair, 48, 0, nil)...), eac3FrameBlocks(0, 0, 7, 1, -1, 64, 0, nil)...), eac3FrameBlocks(1, 0, 2, 0, backPair, 48, 0, nil)...), 8},
		{"JOC (Atmos): a 5.1 core with object metadata, no dependent: 6", eac3FrameBlocks(0, 0, 7, 1, -1, 96, 3, bytes.Repeat([]byte{0xA5}, 40)), 6},
	} {
		if got, ok := ac3FrameChannels(c.frame); !ok || got != c.want {
			t.Errorf("%s: %d channels (ok %v), want %d", c.name, got, ok, c.want)
		}
	}
}
