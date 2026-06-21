package mp4

import (
	"encoding/binary"

	"github.com/gravity-zero/mkvgo/mkv"
)

// codec.go — maps a Matroska track's codec to an ISO-BMFF sample entry plus its
// codec configuration box. mkvgo only remuxes (never transcodes), so a codec is
// supported here only when its MP4 configuration can be produced from the data
// already present in the Matroska track — almost always the CodecPrivate, which
// for the modern codecs IS the MP4 config record verbatim.

// codecSpec describes how one codec is carried in MP4.
type codecSpec struct {
	handler string // mdia/hdlr handler_type: "vide", "soun" or "text"
	video   bool   // true → VisualSampleEntry, false → AudioSampleEntry
	text    bool   // true → timed-text (tx3g) track
	// brand is an optional ftyp compatible-brand to advertise (empty = none).
	brand string
	// needsFirstFrame is true for codecs whose MP4 configuration box is derived
	// from the elementary bitstream (AC-3/E-AC-3/DTS have no CodecPrivate). For
	// those, sampleEntry is called with the track's first sample rather than nil.
	needsFirstFrame bool
	// sampleEntry builds the stsd sample entry box for t, including its child
	// configuration box. firstFrame holds the track's first sample when
	// needsFirstFrame is set, and is nil otherwise. It returns an error when the
	// track lacks the data needed to produce a spec-correct entry.
	sampleEntry func(t *mkv.Track, firstFrame []byte) ([]byte, error)
}

// lookupCodec returns the codecSpec for a Matroska short codec name, or ok=false
// when the codec cannot be remuxed into MP4. The boolean lets the caller
// distinguish "drop this track" from "fail the remux" per its own policy.
func lookupCodec(short string) (codecSpec, bool) {
	s, ok := codecTable[short]
	return s, ok
}

var codecTable = map[string]codecSpec{
	"h264": {handler: "vide", video: true, brand: "avc1", sampleEntry: visualEntry("avc1", "avcC")},
	"hevc": {handler: "vide", video: true, brand: "hvc1", sampleEntry: visualEntry("hvc1", "hvcC")},
	"av1":  {handler: "vide", video: true, brand: "av01", sampleEntry: visualEntry("av01", "av1C")},
	"aac":  {handler: "soun", video: false, sampleEntry: aacEntry},
	"opus": {handler: "soun", video: false, sampleEntry: opusEntry},
	"flac": {handler: "soun", video: false, sampleEntry: flacEntry},
	"ac3":  {handler: "soun", video: false, needsFirstFrame: true, sampleEntry: ac3Entry},
	"eac3": {handler: "soun", video: false, needsFirstFrame: true, sampleEntry: eac3Entry},
	"dts":  {handler: "soun", video: false, sampleEntry: dtsEntry},
	// MP3 in MKV (A_MPEG/L3) is left unmapped by the reader, so it arrives as the
	// raw codec ID.
	"A_MPEG/L3": {handler: "soun", video: false, sampleEntry: mp3Entry},
	// SRT subtitles (S_TEXT/UTF8 → mkvgo short "srt") become tx3g timed text.
	"srt": {handler: "text", text: true, sampleEntry: srtEntry},
}

// isTextSubtitle reports whether a subtitle codec can be carried as MP4 timed
// text (tx3g). Only UTF-8 text (SRT) qualifies; bitmap formats (PGS, VOBSUB)
// and styled formats (ASS/SSA) do not.
func isTextSubtitle(codec string) bool {
	s, ok := codecTable[codec]
	return ok && s.text
}

// visualEntry returns a sampleEntry builder for a video codec whose MKV
// CodecPrivate is exactly the payload of the named MP4 configuration box
// (true for H.264/avcC, HEVC/hvcC and AV1/av1C).
func visualEntry(entryType, configType string) func(*mkv.Track, []byte) ([]byte, error) {
	return func(t *mkv.Track, _ []byte) ([]byte, error) {
		if len(t.CodecPrivate) == 0 {
			return nil, errf("track %d (%s): missing CodecPrivate, cannot build %s", t.ID, t.Codec, configType)
		}
		config := box(configType, t.CodecPrivate)
		return visualSampleEntry(entryType, t, config), nil
	}
}

// visualSampleEntry assembles a VisualSampleEntry (ISO/IEC 14496-12 §12.1.3)
// followed by its codec configuration box.
func visualSampleEntry(typ string, t *mkv.Track, config []byte) []byte {
	return boxf(typ, func(w *bw) {
		w.zeros(6)  // SampleEntry: reserved
		w.u16(1)    // data_reference_index
		w.u16(0)    // pre_defined
		w.u16(0)    // reserved
		w.zeros(12) // pre_defined[3]
		w.u16(uint16(derefU32(t.Width)))
		w.u16(uint16(derefU32(t.Height)))
		w.u32(0x00480000) // horizresolution 72 dpi
		w.u32(0x00480000) // vertresolution 72 dpi
		w.u32(0)          // reserved
		w.u16(1)          // frame_count
		w.zeros(32)       // compressorname (empty Pascal string padding)
		w.u16(0x0018)     // depth
		w.i16(-1)         // pre_defined = 0xFFFF
		w.bytes(config)
		if colr := colrBox(t); colr != nil {
			w.bytes(colr)
		}
	})
}

// colrBox builds a Colour Information box ('colr', type 'nclx') carrying the
// CICP code points so players signal the correct colour space and HDR transfer
// (e.g. BT.2020 + SMPTE-2084 for HDR10). Returns nil when the track has no
// colour information. Absent code points are written as 2 ("unspecified").
func colrBox(t *mkv.Track) []byte {
	if t.ColorPrimaries == nil && t.ColorTransfer == nil && t.ColorSpace == nil && t.ColorRange == nil {
		return nil
	}
	return boxf("colr", func(w *bw) {
		w.fourcc("nclx")
		w.u16(cicp(t.ColorPrimaries))
		w.u16(cicp(t.ColorTransfer))
		w.u16(cicp(t.ColorSpace))
		var fullRange byte
		if t.ColorRange != nil && *t.ColorRange == 2 { // 2 = full/pc range
			fullRange = 0x80
		}
		w.u8(fullRange)
	})
}

func cicp(p *uint16) uint16 {
	if p == nil {
		return 2 // unspecified
	}
	return *p
}

// audioSampleEntry assembles an AudioSampleEntry (ISO/IEC 14496-12 §12.2.3)
// followed by its codec configuration box.
func audioSampleEntry(typ string, t *mkv.Track, config []byte) []byte {
	channels := uint16(2)
	if t.Channels != nil && *t.Channels > 0 {
		channels = uint16(*t.Channels)
	}
	rate := uint32(48000)
	if t.SampleRate != nil && *t.SampleRate > 0 {
		rate = uint32(*t.SampleRate)
	}
	return boxf(typ, func(w *bw) {
		w.zeros(6) // SampleEntry: reserved
		w.u16(1)   // data_reference_index
		w.zeros(8) // reserved (version/revision/vendor in QT; 0 here)
		w.u16(channels)
		w.u16(16) // samplesize
		w.u16(0)  // pre_defined
		w.u16(0)  // reserved
		w.u32(fixed16_16(rate))
		w.bytes(config)
	})
}

// aacEntry builds an mp4a entry. The MKV CodecPrivate is the raw
// AudioSpecificConfig (ISO/IEC 14496-3), which we wrap in an esds descriptor.
func aacEntry(t *mkv.Track, _ []byte) ([]byte, error) {
	if len(t.CodecPrivate) == 0 {
		return nil, errf("track %d (aac): missing CodecPrivate (AudioSpecificConfig)", t.ID)
	}
	return audioSampleEntry("mp4a", t, esdsBox(0x40, t.CodecPrivate)), nil
}

// esdsBox builds the esds (ES Descriptor) box for an audio stream with the given
// objectTypeIndication (0x40 = AAC, 0x6B = MPEG-1 Layer III). asc is the
// DecoderSpecificInfo (AudioSpecificConfig) and may be empty (MP3 has none).
func esdsBox(objType byte, asc []byte) []byte {
	var dc bw
	dc.u8(objType) // objectTypeIndication
	dc.u8(0x15)    // streamType=audio(5)<<2 | upStream(0)<<1 | reserved(1)
	dc.u24(0)      // bufferSizeDB
	dc.u32(0)      // maxBitrate
	dc.u32(0)      // avgBitrate
	if len(asc) > 0 {
		dc.bytes(descriptor(0x05, asc)) // embedded DecoderSpecificInfo
	}
	decConfig := descriptor(0x04, dc.b)

	sl := descriptor(0x06, []byte{0x02}) // SLConfigDescriptor: predefined=MP4

	var es bw
	es.u16(0) // ES_ID
	es.u8(0)  // flags (no URL/OCR/stream priority)
	es.bytes(decConfig)
	es.bytes(sl)
	esDesc := descriptor(0x03, es.b)

	return fullBox("esds", 0, 0, func(w *bw) { w.bytes(esDesc) })
}

// opusEntry builds an "Opus" sample entry. The MKV CodecPrivate is an OpusHead
// (RFC 7845 §5.1); the MP4 dOps box (OpusSpecificBox) carries the same fields
// in big-endian order with the magic and version dropped.
func opusEntry(t *mkv.Track, _ []byte) ([]byte, error) {
	dops, err := dOpsBox(t.CodecPrivate)
	if err != nil {
		return nil, errf("track %d (opus): %v", t.ID, err)
	}
	return audioSampleEntry("Opus", t, dops), nil
}

// dOpsBox converts an OpusHead into an OpusSpecificBox (dOps). OpusHead stores
// PreSkip, InputSampleRate and OutputGain little-endian; dOps stores them
// big-endian. The channel mapping table (when ChannelMappingFamily != 0) is
// copied verbatim.
func dOpsBox(head []byte) ([]byte, error) {
	const minHead = 19 // 8 magic + version + channels + 2 + 4 + 2 + family
	if len(head) < minHead || string(head[:8]) != "OpusHead" {
		return nil, errf("invalid OpusHead in CodecPrivate")
	}
	channels := head[9]
	preSkip := binary.LittleEndian.Uint16(head[10:12])
	inputRate := binary.LittleEndian.Uint32(head[12:16])
	outputGain := binary.LittleEndian.Uint16(head[16:18])
	family := head[18]

	var w bw
	w.u8(0) // OpusSpecificBox Version
	w.u8(channels)
	w.u16(preSkip)
	w.u32(inputRate)
	w.u16(outputGain)
	w.u8(family)
	if family != 0 {
		// StreamCount + CoupledCount + one mapping byte per channel.
		need := 2 + int(channels)
		if len(head) < minHead+need {
			return nil, errf("OpusHead channel mapping table truncated")
		}
		w.bytes(head[19 : 19+need])
	}
	return box("dOps", w.b), nil
}

func derefU32(p *uint32) uint32 {
	if p == nil {
		return 0
	}
	return *p
}
