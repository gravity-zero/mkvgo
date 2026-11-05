package mp4

import (
	"bytes"
	"encoding/binary"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// audio.go - sample entries for audio codecs that need more than a verbatim
// CodecPrivate copy: AC-3 and E-AC-3 (whose MP4 config box is derived from the
// elementary bitstream, since Matroska carries no CodecPrivate for them), plus
// FLAC and MP3.

// --- bit reader / writer (MSB-first) -----------------------------------------

type bitReader struct {
	data []byte
	pos  uint // in bits
	err  bool
}

// bits reads n (<=32) bits big-endian. On overrun it sets err and returns 0.
func (r *bitReader) bits(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		bytePos := r.pos >> 3
		if int(bytePos) >= len(r.data) {
			r.err = true
			return 0
		}
		bit := (r.data[bytePos] >> (7 - (r.pos & 7))) & 1
		v = v<<1 | uint32(bit)
		r.pos++
	}
	return v
}

func (r *bitReader) skip(n int) { r.pos += uint(n) }

type bitWriter struct {
	b     []byte
	cur   byte
	nbits uint
}

func (w *bitWriter) write(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		w.cur |= byte((v>>uint(i))&1) << (7 - w.nbits)
		w.nbits++
		if w.nbits == 8 {
			w.b = append(w.b, w.cur)
			w.cur, w.nbits = 0, 0
		}
	}
}

// bytes flushes any partial byte (zero-padded) and returns the buffer.
func (w *bitWriter) bytes() []byte {
	if w.nbits > 0 {
		w.b = append(w.b, w.cur)
		w.cur, w.nbits = 0, 0
	}
	return w.b
}

// findSync returns the index of the first occurrence of the 16-bit syncword in
// the first maxScan bytes of frame, or -1.
func findSync(frame []byte, sync uint16, maxScan int) int {
	if len(frame) < 2 {
		return -1
	}
	if maxScan > len(frame)-2 {
		maxScan = len(frame) - 2
	}
	for i := 0; i <= maxScan; i++ {
		if uint16(frame[i])<<8|uint16(frame[i+1]) == sync {
			return i
		}
	}
	return -1
}

var ac3SampleRates = [4]uint32{48000, 44100, 32000, 0}

// --- channel counts from codec config (read side) ----------------------------
//
// AudioSampleEntry.channelcount is unreliable for multichannel audio: many MP4
// muxers leave it at 2 regardless of the real layout. The true count lives in the
// codec configuration - the AAC AudioSpecificConfig, or the AC-3/E-AC-3 dac3/dec3
// boxes - which these helpers read. Each returns 0 when the count cannot be
// determined, so the caller keeps the sample-entry value as a fallback.

// ac3AcmodChannels maps an AC-3/E-AC-3 audio coding mode to its full-bandwidth
// channel count (before adding the LFE).
var ac3AcmodChannels = [8]uint8{2, 1, 2, 3, 3, 4, 4, 5}

// aacInfo is everything the AudioSpecificConfig tells us that the AudioSampleEntry
// gets wrong or omits: the real channel count and the decoder's output sample
// rate. parseAACConfig is reader.ParseAACConfig, the one parser both packages use.
type aacInfo struct {
	channels   uint8   // 0 when carried in a program config element
	sampleRate float64 // base/core rate, 0 if not derivable
	outputRate float64 // SBR extension (output) rate, 0 when no SBR
}

func parseAACConfig(asc []byte) aacInfo {
	c := reader.ParseAACConfig(asc)
	return aacInfo{channels: c.Channels, sampleRate: c.SampleRate, outputRate: c.OutputRate}
}

// aacChannels returns the decoder's output channel count for an
// AudioSpecificConfig (accounting for Parametric Stereo). 0 when the layout is in
// a program config element.
func aacChannels(asc []byte) uint8 {
	return parseAACConfig(asc).channels
}

// ac3Channels reads acmod + lfeon from a dac3 (AC3SpecificBox) payload.
func ac3Channels(dac3 []byte) uint8 {
	r := &bitReader{data: dac3}
	r.skip(2 + 5 + 3) // fscod, bsid, bsmod
	acmod := r.bits(3)
	lfeon := r.bits(1)
	if r.err {
		return 0
	}
	return ac3AcmodChannels[acmod] + uint8(lfeon)
}

// eac3ChanLocChannels maps each chan_loc bit (custom dependent-substream channel
// locations, ETSI TS 102 366 Annex E) to the number of channels it carries.
var eac3ChanLocChannels = [9]uint8{2, 2, 1, 1, 2, 2, 2, 1, 1}

// eac3Channels reads the channel count of the first independent substream from a
// dec3 (EC3SpecificBox) payload, including any dependent-substream channels.
func eac3Channels(dec3 []byte) uint8 {
	r := &bitReader{data: dec3}
	r.skip(13)        // data_rate
	r.skip(3)         // num_ind_sub - 1
	r.skip(2 + 5)     // fscod, bsid
	r.skip(1 + 1 + 3) // reserved, asvc, bsmod
	acmod := r.bits(3)
	lfeon := r.bits(1)
	r.skip(3) // reserved
	numDepSub := r.bits(4)
	ch := ac3AcmodChannels[acmod] + uint8(lfeon)
	if numDepSub > 0 {
		chanLoc := r.bits(9)
		for i := 0; i < 9; i++ {
			if chanLoc&(1<<uint(i)) != 0 {
				ch += eac3ChanLocChannels[i]
			}
		}
	}
	if r.err {
		return 0
	}
	return ch
}

// --- AC-3 (ac-3 + dac3) ------------------------------------------------------

func ac3Entry(t *mkv.Track, firstFrame []byte) ([]byte, error) {
	dac3, err := parseAC3(firstFrame)
	if err != nil {
		return nil, errf("track %d (ac3): %w", t.ID, err)
	}
	return audioSampleEntry("ac-3", withStreamChannels(t, ac3Channels(dac3[8:])), dac3), nil
}

// parseAC3 builds a dac3 (AC3SpecificBox) from an AC-3 syncframe (ETSI TS 102
// 366 §4.3 / the MP4 mapping in §F.3).
func parseAC3(frame []byte) ([]byte, error) {
	off := findSync(frame, 0x0B77, 16)
	if off < 0 {
		return nil, errf("no AC-3 syncword")
	}
	r := &bitReader{data: frame[off:]}
	r.skip(16) // syncword
	r.skip(16) // crc1
	fscod := r.bits(2)
	frmsizecod := r.bits(6)
	bsid := r.bits(5)
	if bsid > 8 {
		return nil, errf("bsid %d is not AC-3 (use E-AC-3)", bsid)
	}
	bsmod := r.bits(3)
	acmod := r.bits(3)
	if acmod&0x1 != 0 && acmod != 0x1 {
		r.skip(2) // cmixlev
	}
	if acmod&0x4 != 0 {
		r.skip(2) // surmixlev
	}
	if acmod == 0x2 {
		r.skip(2) // dsurmod
	}
	lfeon := r.bits(1)
	if r.err || fscod == 3 {
		return nil, errf("malformed AC-3 syncframe")
	}
	bitRateCode := frmsizecod >> 1

	var w bitWriter
	w.write(fscod, 2)
	w.write(bsid, 5)
	w.write(bsmod, 3)
	w.write(acmod, 3)
	w.write(lfeon, 1)
	w.write(bitRateCode, 5)
	w.write(0, 5) // reserved
	return box("dac3", w.bytes()), nil
}

// --- E-AC-3 (ec-3 + dec3) ----------------------------------------------------

func eac3Entry(t *mkv.Track, firstFrame []byte) ([]byte, error) {
	dec3, err := parseEAC3(firstFrame)
	if err != nil {
		return nil, errf("track %d (eac3): %w", t.ID, err)
	}
	return audioSampleEntry("ec-3", withStreamChannels(t, eac3Channels(dec3[8:])), dec3), nil
}

var eac3Blocks = [4]uint32{1, 2, 3, 6}

// eac3Substream is what a dec3 says of an independent substream, read from the frame that opens an access unit.
type eac3Substream struct {
	fscod, bsid, bsmod, acmod, lfeon uint32
	kbps                             uint32 // the frame's bit rate
	size                             int    // the frame's bytes, 0 when unknown
}

// eac3Header reads an E-AC-3 syncframe's header at f: strmtyp, substreamid, the substream fields, and, for a
// dependent substream, its chanmap (chanmape false when it has none).
func eac3Header(f []byte) (strmtyp, substream uint32, s eac3Substream, chanmap uint32, chanmape bool, ok bool) {
	r := &bitReader{data: f}
	r.skip(16) // syncword
	strmtyp, substream = r.bits(2), r.bits(3)
	frmsiz := r.bits(11)
	s.fscod = r.bits(2)
	numblkscod := uint32(3)
	if s.fscod == 3 {
		r.skip(2) // fscod2
	} else {
		numblkscod = r.bits(2)
	}
	s.acmod, s.lfeon, s.bsid = r.bits(3), r.bits(1), r.bits(5)
	s.size = int(frmsiz+1) * 2
	if s.fscod < 3 {
		s.kbps = uint32(s.size) * 8 * ac3SampleRates[s.fscod] / (eac3Blocks[numblkscod] * 256 * 1000)
	}
	if strmtyp == 1 {
		r.skip(5) // dialnorm
		if r.bits(1) == 1 {
			r.skip(8) // compr
		}
		if s.acmod == 0 {
			r.skip(5) // dialnorm2
			if r.bits(1) == 1 {
				r.skip(8) // compr2
			}
		}
		if chanmape = r.bits(1) == 1; chanmape {
			chanmap = r.bits(16)
		}
	}
	return strmtyp, substream, s, chanmap, chanmape, !r.err && s.bsid > 10 && s.bsid <= 16
}

// eac3JOC reads the Dolby Atmos signalling of an independent E-AC-3 syncframe at f: the complexity index of its
// joint object coding (flag_ec3_extension_type_a in addbsi, ETSI TS 102 366 E.1.2.2), 0 for none. The BSI is walked
// field by field to reach addbsi.
func eac3JOC(f []byte) uint8 {
	r := &bitReader{data: f}
	r.skip(16)
	strmtyp := r.bits(2)
	r.skip(3 + 11) // substreamid, frmsiz
	fscod := r.bits(2)
	numblkscod := uint32(3)
	if fscod == 3 {
		r.skip(2)
	} else {
		numblkscod = r.bits(2)
	}
	acmod, lfeon := r.bits(3), r.bits(1)
	r.skip(5 + 5) // bsid, dialnorm
	if r.bits(1) == 1 {
		r.skip(8) // compr
	}
	if acmod == 0 {
		r.skip(5)
		if r.bits(1) == 1 {
			r.skip(8)
		}
	}
	if strmtyp == 1 && r.bits(1) == 1 {
		r.skip(16) // chanmap
	}
	if r.bits(1) == 1 { // mixmdate
		if acmod > 2 {
			r.skip(2) // dmixmod
		}
		if acmod&1 != 0 && acmod > 2 {
			r.skip(6) // ltrtcmixlev, lorocmixlev
		}
		if acmod&4 != 0 {
			r.skip(6) // ltrtsurmixlev, lorosurmixlev
		}
		if lfeon == 1 && r.bits(1) == 1 {
			r.skip(5) // lfemixlevcod
		}
		if strmtyp == 0 {
			if r.bits(1) == 1 {
				r.skip(6) // pgmscl
			}
			if acmod == 0 && r.bits(1) == 1 {
				r.skip(6) // pgmscl2
			}
			if r.bits(1) == 1 {
				r.skip(6) // extpgmscl
			}
			switch r.bits(2) { // mixdef
			case 1:
				r.skip(5)
			case 2:
				r.skip(12)
			case 3:
				r.skip(int(r.bits(5)+2) * 8)
			}
			if acmod < 2 {
				if r.bits(1) == 1 {
					r.skip(14) // panmean, paninfo
				}
				if acmod == 0 && r.bits(1) == 1 {
					r.skip(14)
				}
			}
			if r.bits(1) == 1 { // frmmixcfginfoe
				if numblkscod == 0 {
					r.skip(5)
				} else {
					for range eac3Blocks[numblkscod] {
						if r.bits(1) == 1 {
							r.skip(5)
						}
					}
				}
			}
		}
	}
	if r.bits(1) == 1 { // infomdate
		r.skip(3 + 1 + 1) // bsmod, copyrightb, origbs
		if acmod == 2 {
			r.skip(4)
		}
		if acmod >= 6 {
			r.skip(2)
		}
		if r.bits(1) == 1 {
			r.skip(8)
		}
		if acmod == 0 && r.bits(1) == 1 {
			r.skip(8)
		}
		if fscod < 3 {
			r.skip(1)
		}
	}
	if strmtyp == 0 && numblkscod != 3 {
		r.skip(1) // convsync
	}
	if strmtyp == 2 {
		if numblkscod == 3 || r.bits(1) == 1 {
			r.skip(6) // frmsizecod
		}
	}
	if r.bits(1) == 0 { // addbsie
		return 0
	}
	addbsil := r.bits(6)
	r.skip(7)
	if r.bits(1) == 0 || addbsil < 1 { // flag_ec3_extension_type_a, then its complexity index
		return 0
	}
	c := r.bits(8)
	if r.err {
		return 0
	}
	return uint8(c)
}

// entryJOC is the Atmos complexity index an ec-3 sample entry's dec3 carries, 0 for none or another entry.
func entryJOC(entry []byte) uint8 {
	if len(entry) < 8 || string(entry[4:8]) != "ec-3" { // another entry's bytes (an esds copies a CodecPrivate) say nothing
		return 0
	}
	i := bytes.Index(entry[8:], []byte("dec3")) + 8
	if i < 12 {
		return 0
	}
	size := int64(binary.BigEndian.Uint32(entry[i-4:]))
	if size < 8 || size > int64(len(entry)-(i-4)) { // compared in int64: i-4+size wraps an int on 32-bit
		return 0
	}
	p := entry[i+4 : i-4+int(size)]
	if len(p) < 5 || p[1]&0x07 != 0 { // one independent substream: the layout written here
		return 0
	}
	base := 5
	if p[4]>>1&0x0F > 0 { // num_dep_sub (bits 35-38): chan_loc follows
		base = 6
	}
	if len(p) < base+2 || p[base]&1 == 0 {
		return 0
	}
	return p[base+1]
}

// ac3Core reads the BSI of an AC-3 syncframe opening an E-AC-3 access unit (bsid 10 or less).
func ac3Core(f []byte) (eac3Substream, bool) {
	r := &bitReader{data: f}
	r.skip(16 + 16) // syncword, crc1
	var s eac3Substream
	s.fscod = r.bits(2)
	frmsizecod := r.bits(6)
	s.bsid, s.bsmod, s.acmod = r.bits(5), r.bits(3), r.bits(3)
	if s.acmod&0x1 != 0 && s.acmod != 0x1 {
		r.skip(2) // cmixlev
	}
	if s.acmod&0x4 != 0 {
		r.skip(2) // surmixlev
	}
	if s.acmod == 0x2 {
		r.skip(2) // dsurmod
	}
	s.lfeon = r.bits(1)
	if r.err || s.fscod == 3 || frmsizecod>>1 >= uint32(len(ac3Bitrates)) {
		return s, false
	}
	s.kbps = ac3Bitrates[frmsizecod>>1]
	s.size = ac3SyncframeBytes(s.fscod, frmsizecod)
	return s, true
}

// ac3SyncframeBytes is an AC-3 syncframe's size from its fscod and frmsizecod.
func ac3SyncframeBytes(fscod, frmsizecod uint32) int {
	kbps := int(ac3Bitrates[frmsizecod>>1])
	switch fscod {
	case 0:
		return 4 * kbps
	case 1: // 44.1 kHz: rounded down to words, odd codes one word more
		return 2 * (kbps*1000*1536/44100/16 + int(frmsizecod&1))
	}
	return 6 * kbps
}

// eac3ChanmapLoc is the chan_loc bit of each chanmap position, most significant first: -1 for the positions an
// independent substream's acmod and lfeon place, and for Lts/Rts, which chan_loc has no bit for.
var eac3ChanmapLoc = [16]int8{-1, -1, -1, -1, -1, 0, 1, 2, 3, 4, 5, 6, 7, -1, 8, -1}

// chanLocCs is chan_loc's back centre bit, the one position an acmod places that chan_loc also names.
const chanLocCs = 1 << 2

// acmodHasCs reports an acmod with a back centre (2/1, 3/1).
func acmodHasCs(acmod uint32) bool { return acmod == 4 || acmod == 5 }

// parseEAC3 builds a dec3 (EC3SpecificBox, ETSI TS 102 366 Annex F) from the access unit opening frame: its
// independent substream - an E-AC-3 one, or an AC-3 core read as AC-3 - and the dependent substreams after it, whose
// channel locations chan_loc gathers, so a 7.1 (a 5.1 and the rear pair) says 8 channels as it decodes; and the
// Atmos (JOC) complexity index its addbsi signals.
func parseEAC3(frame []byte) ([]byte, error) {
	off := findSync(frame, 0x0B77, 16)
	if off < 0 || off+6 > len(frame) {
		return nil, errf("no E-AC-3 syncword")
	}
	f := frame[off:]
	var ind eac3Substream
	var joc uint8
	if f[5]>>3 <= 10 {
		core, ok := ac3Core(f)
		if !ok {
			return nil, errf("malformed AC-3 core of an E-AC-3 syncframe")
		}
		ind = core
	} else {
		strmtyp, _, s, _, _, ok := eac3Header(f)
		if !ok || strmtyp == 1 {
			return nil, errf("malformed E-AC-3 syncframe")
		}
		ind = s
		joc = eac3JOC(f)
	}
	dataRate, numDep, chanLoc := ind.kbps, uint32(0), uint32(0)
	for p := ind.size; p > 0 && p+6 <= len(f) && f[p] == 0x0B && f[p+1] == 0x77 && numDep < 15; {
		strmtyp, _, dep, chanmap, chanmape, ok := eac3Header(f[p:])
		if !ok || strmtyp != 1 {
			break // the next access unit, or another programme
		}
		numDep++
		dataRate += dep.kbps
		if chanmape {
			for i, loc := range eac3ChanmapLoc {
				if loc >= 0 && chanmap&(1<<(15-i)) != 0 {
					chanLoc |= 1 << loc
				}
			}
		} else if acmodHasCs(dep.acmod) {
			chanLoc |= chanLocCs
		}
		p += dep.size
	}
	if acmodHasCs(ind.acmod) {
		chanLoc &^= chanLocCs
	}

	var w bitWriter
	w.write(dataRate, 13)
	w.write(0, 3) // num_ind_sub - 1 (one independent substream)
	w.write(ind.fscod, 2)
	w.write(ind.bsid, 5)
	w.write(0, 1) // reserved
	w.write(0, 1) // asvc
	w.write(ind.bsmod, 3)
	w.write(ind.acmod, 3)
	w.write(ind.lfeon, 1)
	w.write(0, 3) // reserved
	w.write(numDep, 4)
	if numDep > 0 {
		w.write(chanLoc, 9)
	} else {
		w.write(0, 1) // reserved
	}
	if joc > 0 { // Dolby Atmos: flag_ec3_extension_type_a and complexity_index_type_a after the substreams
		w.write(0, 7)
		w.write(1, 1)
		w.write(uint32(joc), 8)
	}
	return box("dec3", w.bytes()), nil
}

// --- FLAC (fLaC + dfLa) ------------------------------------------------------

func flacEntry(t *mkv.Track, _ []byte) ([]byte, error) {
	if len(t.CodecPrivate) == 0 {
		return nil, errf("track %d (flac): missing CodecPrivate", t.ID)
	}
	// The MKV FLAC CodecPrivate is the FLAC stream: the "fLaC" marker followed by
	// metadata blocks. dfLa carries the metadata blocks only.
	meta := t.CodecPrivate
	if len(meta) >= 4 && string(meta[:4]) == "fLaC" {
		meta = meta[4:]
	}
	dfla := fullBox("dfLa", 0, 0, func(w *bw) { w.bytes(meta) })
	return audioSampleEntry("fLaC", t, dfla), nil
}

// --- DTS (mp4a + esds, objectTypeIndication 0xA9) ----------------------------

// dtsEntry carries DTS (Coherent Acoustics core, and DTS-HD streams whose first
// substream is a core) as mp4a + esds with objectTypeIndication 0xA9, matching
// the way the de-facto mov muxer stores DTS. The decoder reads the core and any
// extension substreams from the frames themselves, so no config payload is
// needed; DTS-HD's extension data rides along in the verbatim samples.
func dtsEntry(t *mkv.Track, _ []byte) ([]byte, error) {
	return audioSampleEntry("mp4a", t, esdsBox(0xA9, nil)), nil
}

// --- MP3 (mp4a + esds, objectTypeIndication 0x6B) ----------------------------

func mp3Entry(t *mkv.Track, _ []byte) ([]byte, error) {
	// MPEG-1/2 Layer III has no DecoderSpecificInfo; the decoder reads the frame
	// headers. objectTypeIndication 0x6B = Audio ISO/IEC 11172-3.
	return audioSampleEntry("mp4a", t, esdsBox(0x6B, nil)), nil
}
