package mp4

import "github.com/gravity-zero/mkvgo/mkv"

// audio.go — sample entries for audio codecs that need more than a verbatim
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
// codec configuration — the AAC AudioSpecificConfig, or the AC-3/E-AC-3 dac3/dec3
// boxes — which these helpers read. Each returns 0 when the count cannot be
// determined, so the caller keeps the sample-entry value as a fallback.

// ac3AcmodChannels maps an AC-3/E-AC-3 audio coding mode to its full-bandwidth
// channel count (before adding the LFE).
var ac3AcmodChannels = [8]uint8{2, 1, 2, 3, 3, 4, 4, 5}

// aacConfigChannels maps an AAC channelConfiguration to a channel count. Index 0
// means the layout is carried in a program config element (not resolved here).
var aacConfigChannels = [8]uint8{0, 1, 2, 3, 4, 5, 6, 8}

// aacChannels returns the number of channels an AAC decoder configured by asc
// (an AudioSpecificConfig) produces. It is more than the front
// channelConfiguration: a Parametric Stereo stream (HE-AACv2) codes a mono core
// — channelConfiguration 1 — but the decoder reconstructs a stereo signal, so
// ffmpeg/ffprobe report 2 channels. Both signalling forms are detected:
//
//   - explicit hierarchical: audioObjectType 29 (PS) up front;
//   - backward-compatible: a trailing sync extension (0x2b7 → SBR, then an inner
//     0x548 → PS) after the GASpecificConfig.
//
// Returns 0 when the channel layout is carried in a program config element
// (channelConfiguration 0), so the caller keeps the sample-entry value.
func aacChannels(asc []byte) uint8 {
	r := &bitReader{data: asc}
	aot := getAudioObjectType(r)
	if r.bits(4) == 0xF { // samplingFrequencyIndex == 0xF → explicit 24-bit rate
		r.skip(24)
	}
	cc := r.bits(4)

	ps := false
	explicitExt := false
	if aot == 5 || aot == 29 { // SBR or PS signalled hierarchically up front
		explicitExt = true
		if aot == 29 {
			ps = true
		}
		if r.bits(4) == 0xF { // extensionSamplingFrequencyIndex
			r.skip(24)
		}
		aot = getAudioObjectType(r) // underlying object type
		if aot == 22 {              // ER BSAC carries an extension channel config
			r.skip(4)
		}
	}

	// Backward-compatible signalling rides as a sync extension after the
	// GASpecificConfig. ffmpeg only looks for it when SBR was not already signalled
	// explicitly, so walk the GASpecificConfig to position the reader, then probe.
	if !explicitExt && isGAObjectType(aot) && skipGASpecificConfig(r, aot, cc) {
		if bitsLeft(r) >= 16 && r.bits(11) == 0x2b7 { // syncExtensionType: SBR
			if getAudioObjectType(r) == 5 && r.bits(1) == 1 { // ext AOT SBR + sbrPresentFlag
				if r.bits(4) == 0xF { // extensionSamplingFrequencyIndex
					r.skip(24)
				}
				if bitsLeft(r) >= 12 && r.bits(11) == 0x548 { // syncExtensionType: PS
					ps = r.bits(1) == 1 // psPresentFlag
				}
			}
		}
	}

	return aacChannelsFrom(cc, ps)
}

// aacChannelsFrom resolves a channelConfiguration plus a Parametric Stereo flag
// to the decoder's output channel count: a PS stream over a mono core yields 2.
func aacChannelsFrom(cc uint32, ps bool) uint8 {
	if cc == 0 || cc >= uint32(len(aacConfigChannels)) {
		return 0
	}
	ch := aacConfigChannels[cc]
	if ps && ch == 1 {
		ch = 2
	}
	return ch
}

// getAudioObjectType reads an AAC audioObjectType: 5 bits, or 5+6 (escape) when
// the first five are all ones.
func getAudioObjectType(r *bitReader) uint32 {
	aot := r.bits(5)
	if aot == 31 {
		aot = 32 + r.bits(6)
	}
	return aot
}

// bitsLeft reports how many bits remain unread in r.
func bitsLeft(r *bitReader) int {
	return len(r.data)*8 - int(r.pos)
}

// isGAObjectType reports whether aot uses a GASpecificConfig (the General Audio
// object types whose config skipGASpecificConfig can walk).
func isGAObjectType(aot uint32) bool {
	switch aot {
	case 1, 2, 3, 4, 6, 7, 17, 19, 20, 21, 22, 23:
		return true
	}
	return false
}

// skipGASpecificConfig advances r past a GASpecificConfig (ISO/IEC 14496-3
// §4.4.1) so the reader is positioned at any trailing sync extension. It returns
// false — and leaves the position unusable — when the layout cannot be walked
// (a program config element) or the buffer runs out.
func skipGASpecificConfig(r *bitReader, aot, cc uint32) bool {
	if cc == 0 {
		return false // program_config_element: not walked
	}
	r.skip(1)           // frameLengthFlag
	if r.bits(1) == 1 { // dependsOnCoreCoder
		r.skip(14) // coreCoderDelay
	}
	extensionFlag := r.bits(1)
	if aot == 6 || aot == 20 {
		r.skip(3) // layerNr
	}
	if extensionFlag == 1 {
		if aot == 22 {
			r.skip(5 + 11) // numOfSubFrame + layer_length
		}
		if aot == 17 || aot == 19 || aot == 20 || aot == 23 {
			r.skip(3) // section/scalefactor/spectral data resilience flags
		}
		r.skip(1) // extensionFlag3
	}
	return !r.err
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
		return nil, errf("track %d (ac3): %v", t.ID, err)
	}
	return audioSampleEntry("ac-3", t, dac3), nil
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
		return nil, errf("track %d (eac3): %v", t.ID, err)
	}
	return audioSampleEntry("ec-3", t, dec3), nil
}

var eac3Blocks = [4]uint32{1, 2, 3, 6}

// parseEAC3 builds a dec3 (EC3SpecificBox) describing a single independent
// substream from the first E-AC-3 syncframe (ETSI TS 102 366 Annex E).
func parseEAC3(frame []byte) ([]byte, error) {
	off := findSync(frame, 0x0B77, 16)
	if off < 0 {
		return nil, errf("no E-AC-3 syncword")
	}
	r := &bitReader{data: frame[off:]}
	r.skip(16) // syncword
	r.skip(2)  // strmtyp
	r.skip(3)  // substreamid
	frmsiz := r.bits(11)
	fscod := r.bits(2)
	var numblkscod uint32
	if fscod == 3 {
		r.skip(2) // fscod2
		numblkscod = 3
	} else {
		numblkscod = r.bits(2)
	}
	acmod := r.bits(3)
	lfeon := r.bits(1)
	bsid := r.bits(5)
	if r.err {
		return nil, errf("malformed E-AC-3 syncframe")
	}

	// Nominal data rate in kbit/s, derived from the frame size and duration.
	dataRate := uint32(0)
	if fscod < 3 {
		sr := ac3SampleRates[fscod]
		samples := eac3Blocks[numblkscod] * 256
		if samples > 0 {
			frameBytes := (frmsiz + 1) * 2
			dataRate = frameBytes * 8 * sr / (samples * 1000)
		}
	}

	var w bitWriter
	w.write(dataRate, 13)
	w.write(0, 3) // num_ind_sub - 1 (one independent substream)
	// independent substream 0:
	w.write(fscod, 2)
	w.write(bsid, 5)
	w.write(0, 1) // reserved
	w.write(0, 1) // asvc
	w.write(0, 3) // bsmod (not in the main bsi; default to main service)
	w.write(acmod, 3)
	w.write(lfeon, 1)
	w.write(0, 3) // reserved
	w.write(0, 4) // num_dep_sub
	w.write(0, 1) // reserved (num_dep_sub == 0)
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
// the way ffmpeg's mov muxer stores DTS. The decoder reads the core and any
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
