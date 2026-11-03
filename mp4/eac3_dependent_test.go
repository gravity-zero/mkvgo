package mp4

import (
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// eac3SubFrame is an E-AC-3 syncframe of size bytes (48 kHz, 6 blocks, bsid 16): strmtyp, substreamid, acmod, lfeon,
// then, for a dependent substream, chanmap when not negative. Only the header is read; the rest is zeros.
func eac3SubFrame(strmtyp, substream, acmod, lfe uint32, chanmap int, size int) []byte {
	var w bitWriter
	w.write(0x0B77, 16)
	w.write(strmtyp, 2)
	w.write(substream, 3)
	w.write(uint32(size/2-1), 11)
	w.write(0, 2) // fscod 48 kHz
	w.write(3, 2) // numblkscod: 6 blocks
	w.write(acmod, 3)
	w.write(lfe, 1)
	w.write(16, 5) // bsid
	w.write(31, 5) // dialnorm
	w.write(0, 1)  // compre
	if acmod == 0 {
		w.write(31, 5)
		w.write(0, 1)
	}
	if strmtyp == 1 {
		if chanmap >= 0 {
			w.write(1, 1)
			w.write(uint32(chanmap), 16)
		} else {
			w.write(0, 1)
		}
	}
	f := w.bytes()
	return append(f, make([]byte, size-len(f))...)
}

// ac3CoreSyncFrame is a 48 kHz 448 kbit/s AC-3 syncframe (1792 bytes) of acmod, lfeon and bsmod, bsid 6.
func ac3CoreSyncFrame(bsmod, acmod, lfe uint32) []byte {
	var w bitWriter
	w.write(0x0B77, 16)
	w.write(0, 16) // crc1
	w.write(0, 2)  // fscod 48 kHz
	w.write(30, 6) // frmsizecod 448 kbit/s
	w.write(6, 5)  // bsid
	w.write(bsmod, 3)
	w.write(acmod, 3)
	if acmod&1 != 0 && acmod != 1 {
		w.write(0, 2)
	}
	if acmod&4 != 0 {
		w.write(0, 2)
	}
	if acmod == 2 {
		w.write(0, 2)
	}
	w.write(lfe, 1)
	f := w.bytes()
	return append(f, make([]byte, 1792-len(f))...)
}

// dec3Fields are the fields of a dec3 payload's first independent substream.
type dec3Fields struct {
	dataRate, fscod, bsid, bsmod, acmod, lfeon, numDepSub, chanLoc uint32
}

func readDec3(t *testing.T, dec3 []byte) dec3Fields {
	t.Helper()
	r := &bitReader{data: dec3[8:]}
	var f dec3Fields
	f.dataRate = r.bits(13)
	r.skip(3)
	f.fscod, f.bsid = r.bits(2), r.bits(5)
	r.skip(2)
	f.bsmod, f.acmod, f.lfeon = r.bits(3), r.bits(3), r.bits(1)
	r.skip(3)
	if f.numDepSub = r.bits(4); f.numDepSub > 0 {
		f.chanLoc = r.bits(9)
	}
	if r.err {
		t.Fatal("dec3 too short")
	}
	return f
}

// chanmapLrsRrs is a dependent substream's chanmap placing its channels on the rear surround pair (Lrs/Rrs).
const chanmapLrsRrs = 1 << (15 - 6)

// An E-AC-3 7.1 is a 5.1 independent substream and a dependent one with the rear pair: the dec3 lists the dependent
// and its channel location, so the init announces 8 channels, as the stream decodes.
func TestEAC3DependentSubstreamInDec3(t *testing.T) {
	au := append(eac3SubFrame(0, 0, 7, 1, -1, 768), eac3SubFrame(1, 0, 2, 0, chanmapLrsRrs, 256)...)
	dec3, err := parseEAC3(au)
	if err != nil {
		t.Fatal(err)
	}
	f := readDec3(t, dec3)
	if f.acmod != 7 || f.lfeon != 1 || f.numDepSub != 1 || f.chanLoc != 1<<1 {
		t.Fatalf("dec3 = %+v, want acmod 7, lfeon 1, one dependent at chan_loc Lrs/Rrs", f)
	}
	if got := eac3Channels(dec3[8:]); got != 8 {
		t.Fatalf("eac3Channels = %d, want 8", got)
	}
	ch := uint8(6)
	entry, err := eac3Entry(&mkv.Track{ID: 1, Codec: "eac3", Channels: &ch}, au)
	if err != nil {
		t.Fatal(err)
	}
	if got := uint16(entry[24])<<8 | uint16(entry[25]); got != 8 {
		t.Fatalf("ec-3 channelcount = %d, want 8", got)
	}
}

// A dependent substream without a chanmap adds the positions its acmod has and the independent one lacks: a 3/2
// core with a 3/2/1 dependent gains the back centre.
func TestEAC3DependentWithoutChanmap(t *testing.T) {
	au := append(eac3SubFrame(0, 0, 7, 1, -1, 768), eac3SubFrame(1, 0, 5, 0, -1, 256)...)
	dec3, err := parseEAC3(au)
	if err != nil {
		t.Fatal(err)
	}
	if f := readDec3(t, dec3); f.numDepSub != 1 || f.chanLoc != 1<<2 {
		t.Fatalf("dec3 = %+v, want one dependent at chan_loc Cs", f)
	}
}

// An E-AC-3 track whose access units open with an AC-3 core (bsid 6) gets its dec3 from the AC-3 BSI - fscod, bsid,
// bsmod, acmod, lfeon read where AC-3 puts them - and the dependents after the core.
func TestEAC3WithAC3Core(t *testing.T) {
	for _, tc := range []struct {
		name    string
		au      []byte
		want    dec3Fields
		channel uint8
	}{
		{"5.1 core alone", ac3CoreSyncFrame(2, 7, 1), dec3Fields{dataRate: 448, bsid: 6, bsmod: 2, acmod: 7, lfeon: 1}, 6},
		{"2.0 core alone", ac3CoreSyncFrame(0, 2, 0), dec3Fields{dataRate: 448, bsid: 6, acmod: 2}, 2},
		{"5.1 core and the rear pair", append(ac3CoreSyncFrame(0, 7, 1), eac3SubFrame(1, 0, 2, 0, chanmapLrsRrs, 256)...),
			dec3Fields{dataRate: 512, bsid: 6, acmod: 7, lfeon: 1, numDepSub: 1, chanLoc: 1 << 1}, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec3, err := parseEAC3(tc.au)
			if err != nil {
				t.Fatal(err)
			}
			if got := readDec3(t, dec3); got != tc.want {
				t.Fatalf("dec3 = %+v, want %+v", got, tc.want)
			}
			if got := eac3Channels(dec3[8:]); got != tc.channel {
				t.Fatalf("eac3Channels = %d, want %d", got, tc.channel)
			}
		})
	}
}
