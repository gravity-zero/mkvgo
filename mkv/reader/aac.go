package reader

import "github.com/gravity-zero/mkvgo/mkv"

// AACConfig is what an AudioSpecificConfig (ISO/IEC 14496-3 §1.6) says about
// the stream that the container fields get wrong or omit.
type AACConfig struct {
	// ObjectType is the audioObjectType up front (5 = SBR, 29 = PS when the
	// extension is signalled hierarchically) - what an mp4a.40.N codec string
	// carries. CoreObjectType is the underlying coder (2 = LC) once an
	// extension is unwrapped.
	ObjectType, CoreObjectType uint32
	// Profile is the conventional prober spelling: "LC", "Main", "SSR", "LTP",
	// "LD", "ELD", "HE-AAC" (SBR), "HE-AACv2" (SBR + Parametric Stereo). ""
	// for an object type without a conventional name.
	Profile string
	// Channels is the decoder's output count (PS over a mono core = 2); 0 when
	// the layout is in a program config element.
	Channels uint8
	// SampleRate is the core rate; OutputRate the SBR extension (output) rate,
	// 0 when no SBR is signalled.
	SampleRate, OutputRate float64
	SBR, PS                bool
	// FrameLength is the samples a core frame covers (1024, or 960 when the
	// GASpecificConfig sets frameLengthFlag); 1024 when the config does not say.
	FrameLength uint32
}

// aacConfigChannels maps an AAC channelConfiguration to a channel count. Index
// 0 means "in a program config element" (unknown here).
var aacConfigChannels = [8]uint8{0, 1, 2, 3, 4, 5, 6, 8}

// aacSampleRates maps a 4-bit samplingFrequencyIndex to a rate in Hz; indices
// 13-15 are reserved (0).
var aacSampleRates = [16]uint32{
	96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
	16000, 12000, 11025, 8000, 7350, 0, 0, 0,
}

// ParseAACConfig walks an AudioSpecificConfig and reports the decoder's
// effective profile, channel count and sample rates. Two things make the front
// fields insufficient:
//
//   - Parametric Stereo (HE-AACv2) codes a mono core (channelConfiguration 1)
//     the decoder upmixes to stereo → 2 channels, like external probers.
//   - SBR (HE-AAC) codes a half-rate core the decoder doubles → probers report
//     the extensionSamplingFrequency, not the core rate.
//
// Both the explicit hierarchical form (audioObjectType 5 = SBR, 29 = PS up
// front) and the backward-compatible trailing sync extension (0x2b7 → SBR,
// 0x548 → PS) are detected.
//
// Limitation: when SBR or Parametric Stereo is signalled only in-band (in the
// audio frames, not the ASC), it is invisible from the head - a plain AAC-LC
// ASC (0x1310) whose frames carry SBR reads as LC at the core rate, and an
// explicit-SBR mono core with in-band PS (0x2b8a0800) reads as 1 channel. The
// data is in no header; a probe that decodes a frame sees more. Safe on any
// input: a truncated config yields its trustworthy front fields.
func ParseAACConfig(asc []byte) AACConfig {
	r := &bitReader{data: asc}
	aot := getAudioObjectType(r)
	c := AACConfig{ObjectType: aot, CoreObjectType: aot, FrameLength: 1024}
	baseRate := readSamplingFrequency(r)
	cc := r.bits(4)

	outputRate := float64(0)
	explicitExt := false
	if aot == 5 || aot == 29 { // SBR or PS signalled hierarchically up front
		explicitExt = true
		c.SBR = true
		c.PS = aot == 29
		outputRate = readSamplingFrequency(r) // extensionSamplingFrequency
		c.CoreObjectType = getAudioObjectType(r)
		if c.CoreObjectType == 22 { // ER BSAC carries an extension channel config
			r.bits(4)
		}
	}

	// Backward-compatible signalling rides as a sync extension after the
	// GASpecificConfig. Mainstream decoders only look for it when SBR was not
	// already signalled explicitly, so walk the GASpecificConfig to position the
	// reader, then probe.
	if !explicitExt && isGAObjectType(aot) && skipGASpecificConfig(r, aot, cc, &c.FrameLength) {
		if bitsLeft(r) >= 16 && r.bits(11) == 0x2b7 { // syncExtensionType: SBR
			if getAudioObjectType(r) == 5 && r.bits(1) == 1 { // ext AOT SBR + sbrPresentFlag
				c.SBR = true
				outputRate = readSamplingFrequency(r)         // extensionSamplingFrequency
				if bitsLeft(r) >= 12 && r.bits(11) == 0x548 { // syncExtensionType: PS
					c.PS = r.bits(1) == 1 // psPresentFlag
				}
			}
		}
	}

	if r.err || !c.SBR {
		// A short/partial parse still yields the trustworthy front fields.
		outputRate = 0
	}
	c.SampleRate = baseRate
	c.OutputRate = outputRate
	c.Channels = aacChannelsFrom(cc, c.PS)
	c.Profile = aacProfileName(c.CoreObjectType, c.SBR, c.PS)
	return c
}

// aacProfileName is the conventional prober spelling of an AAC profile: the
// core object type's name, or the HE-AAC names when SBR (and PS) are signalled.
func aacProfileName(core uint32, sbr, ps bool) string {
	switch {
	case sbr && ps:
		return "HE-AACv2"
	case sbr:
		return "HE-AAC"
	}
	switch core {
	case 1:
		return "Main"
	case 2:
		return "LC"
	case 3:
		return "SSR"
	case 4:
		return "LTP"
	case 23:
		return "LD"
	case 39:
		return "ELD"
	}
	return ""
}

// readSamplingFrequency reads a 4-bit samplingFrequencyIndex (or the explicit
// 24-bit rate when the index is 0xF) and returns the rate in Hz, 0 if reserved.
func readSamplingFrequency(r *bitReader) float64 {
	idx := r.bits(4)
	if idx == 0xF {
		return float64(r.bits(24))
	}
	return float64(aacSampleRates[idx])
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
	return len(r.data)*8 - r.pos
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
// false - and leaves the position unusable - when the layout cannot be walked
// (a program config element) or the buffer runs out.
func skipGASpecificConfig(r *bitReader, aot, cc uint32, frameLength *uint32) bool {
	if cc == 0 {
		return false // program_config_element: not walked
	}
	if r.bits(1) == 1 { // frameLengthFlag: 960-sample frames
		*frameLength = 960
	}
	if r.bits(1) == 1 { // dependsOnCoreCoder
		r.bits(14) // coreCoderDelay
	}
	extensionFlag := r.bits(1)
	if aot == 6 || aot == 20 {
		r.bits(3) // layerNr
	}
	if extensionFlag == 1 {
		if aot == 22 {
			r.bits(5 + 11) // numOfSubFrame + layer_length
		}
		if aot == 17 || aot == 19 || aot == 20 || aot == 23 {
			r.bits(3) // section/scalefactor/spectral data resilience flags
		}
		r.bits(1) // extensionFlag3
	}
	return !r.err
}

func isAACCodec(codec string) bool {
	switch codec {
	case "aac", "A_AAC":
		return true
	}
	return codec == "A_AAC/MPEG4/LC" || len(codec) > 6 && codec[:6] == "A_AAC/"
}

// fillAACFromASC fills what an AAC track's AudioSpecificConfig states and the
// container did not: the profile (never in the container), the SBR output rate
// (OutputSamplingFrequency, which mainstream muxers leave out even for
// explicit SBR), the channel count and core rate when absent. Container values
// win per field, as everywhere.
func fillAACFromASC(t *mkv.Track) {
	if t.Type != mkv.AudioTrack || !isAACCodec(t.Codec) || len(t.CodecPrivate) == 0 {
		return
	}
	c := ParseAACConfig(t.CodecPrivate)
	if t.Profile == "" {
		t.Profile = c.Profile
	}
	if t.Channels == nil && c.Channels > 0 {
		ch := c.Channels
		t.Channels = &ch
	}
	if t.SampleRate == nil && c.SampleRate > 0 {
		sr := c.SampleRate
		t.SampleRate = &sr
	}
	if t.OutputSampleRate == nil && c.OutputRate > 0 {
		osr := c.OutputRate
		t.OutputSampleRate = &osr
	}
}
