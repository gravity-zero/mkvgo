package mp4

import (
	"bytes"
	"encoding/binary"
	"strings"

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

	// Timed-text subtitle encoders (set only when text is true). cueSample turns
	// one Matroska subtitle block's payload into an MP4 sample; emptySample is the
	// gap/lead-in/bridge sample that keeps the text track continuous.
	cueSample   func(payload []byte) []byte
	emptySample []byte
}

// lookupCodec returns the codecSpec for a Matroska short codec name, or ok=false
// when the codec cannot be remuxed into MP4. The boolean lets the caller
// distinguish "drop this track" from "fail the remux" per its own policy.
func lookupCodec(short string) (codecSpec, bool) {
	s, ok := codecTable[short]
	return s, ok
}

var codecTable = map[string]codecSpec{
	"h264": {handler: "vide", video: true, brand: "avc1", sampleEntry: visualEntry("avc1", "dva1", "avcC")},
	"hevc": {handler: "vide", video: true, brand: "hvc1", sampleEntry: visualEntry("hvc1", "dvh1", "hvcC")},
	"av1":  {handler: "vide", video: true, brand: "av01", sampleEntry: visualEntry("av01", "dav1", "av1C")},
	// VP9 in MP4 (vp09 + vpcC, ISO/IEC 14496-15 style per the VP9-in-ISOBMFF
	// spec). The vpcC is built from the first keyframe's uncompressed header
	// when the Matroska track carries no VPCodecConfigurationRecord.
	"vp9":  {handler: "vide", video: true, brand: "vp09", needsFirstFrame: true, sampleEntry: vp9Entry},
	"aac":  {handler: "soun", video: false, sampleEntry: aacEntry},
	"opus": {handler: "soun", video: false, sampleEntry: opusEntry},
	"flac": {handler: "soun", video: false, sampleEntry: flacEntry},
	"ac3":  {handler: "soun", video: false, needsFirstFrame: true, sampleEntry: ac3Entry},
	"eac3": {handler: "soun", video: false, needsFirstFrame: true, sampleEntry: eac3Entry},
	"dts":  {handler: "soun", video: false, sampleEntry: dtsEntry},
	// MP3 in MKV (A_MPEG/L3) is left unmapped by the reader, so it arrives as the
	// raw codec ID.
	"A_MPEG/L3": {handler: "soun", video: false, sampleEntry: mp3Entry},
	// SRT (S_TEXT/UTF8) becomes tx3g timed text. This entry is also the carriage
	// used by default for WebVTT and for flattened ASS/SSA — tx3g is the only
	// MP4 subtitle form universally read by players (ffmpeg included).
	"srt": {handler: "text", text: true, sampleEntry: srtEntry, cueSample: encodeCue, emptySample: emptyCue},
}

// wvttSpec carries WebVTT losslessly as native wvtt (ISO/IEC 14496-30). It is
// opt-in (Options.NativeWebVTT): wvtt preserves cue settings and markup and is
// read by Apple/Safari/CMAF, but ffmpeg's MP4 demuxer does not recognise it, so
// it is not the default.
var wvttSpec = codecSpec{handler: "text", text: true, sampleEntry: wvttEntry,
	cueSample: encodeWVTTCue, emptySample: wvttEmptyCue}

// subtitleCarriage returns how a Matroska subtitle codec is carried into MP4.
//
//   - SRT and WebVTT are carried as tx3g by default — the only MP4 subtitle form
//     read universally (ffmpeg included). WebVTT uses native lossless wvtt instead
//     when nativeWebVTT is set (Apple/CMAF; ffmpeg cannot read it).
//   - ASS/SSA have no plain-text-safe default and are dropped unless flatten is
//     set, which carries them as tx3g (all styling/positioning lost).
//   - Bitmap formats (PGS/VOBSUB) have no MP4 timed-text form and are dropped.
//
// ok=false means "drop this track".
func subtitleCarriage(codec string, flatten, nativeWebVTT bool) (codecSpec, bool) {
	switch canonicalSubCodec(codec) {
	case "srt":
		return codecTable["srt"], true
	case "webvtt":
		if nativeWebVTT {
			return wvttSpec, true
		}
		return codecTable["srt"], true
	case "ass", "ssa":
		if !flatten {
			return codecSpec{}, false
		}
		s := codecTable["srt"]
		s.cueSample = func(p []byte) []byte { return encodeCue(flattenASS(p)) }
		return s, true
	default:
		return codecSpec{}, false
	}
}

// canonicalSubCodec folds the WebM-era WebVTT codec IDs (D_WEBVTT/SUBTITLES,
// /CAPTIONS, /DESCRIPTIONS, /METADATA — written by some muxers, including ffmpeg)
// into the canonical "webvtt" short name, so they are carried like S_TEXT/WEBVTT
// rather than dropped.
func canonicalSubCodec(codec string) string {
	if strings.HasPrefix(codec, "D_WEBVTT/") {
		return "webvtt"
	}
	return codec
}

// subtitleDropReason explains why a subtitle codec could not be carried, so the
// reason surfaced via Options.OnDrop tells the user what to do.
func subtitleDropReason(codec string) string {
	switch canonicalSubCodec(codec) {
	case "ass", "ssa":
		return "styled subtitles (ASS/SSA) have no native MP4 form; set Options.FlattenStyledSubs to carry them as plain timed text (styling is lost)"
	default:
		return "subtitle format not representable as MP4 timed text"
	}
}

// visualEntry returns a sampleEntry builder for a video codec whose MKV
// CodecPrivate is exactly the payload of the named MP4 configuration box
// (true for H.264/avcC, HEVC/hvcC and AV1/av1C).
func visualEntry(entryType, dvEntryType, configType string) func(*mkv.Track, []byte) ([]byte, error) {
	return func(t *mkv.Track, _ []byte) ([]byte, error) {
		if len(t.CodecPrivate) == 0 {
			return nil, errf("track %d (%s): missing CodecPrivate, cannot build %s", t.ID, t.Codec, configType)
		}
		et := entryType
		// A non-cross-compatible Dolby Vision stream (bl_signal_compatibility_id 0,
		// e.g. profile 5/7) needs the Dolby sample entry type — its base layer is not
		// a standard HEVC/AVC/AV1 stream, so a plain hvc1/avc1/av01 tag would mislead
		// a non-DV decoder. Cross-compatible profiles (profile 8) keep the plain tag.
		if t.DolbyVision != nil && t.DolbyVision.BLSignalCompatID == 0 {
			et = dvEntryType
		}
		config := box(configType, t.CodecPrivate)
		return visualSampleEntry(et, t, config), nil
	}
}

// vp9Entry builds a vp09 VisualSampleEntry. The vpcC configuration record comes
// from the track's CodecPrivate when it already holds one (some muxers store
// it), and is otherwise derived from the first keyframe's uncompressed header
// (profile, bit depth, chroma subsampling, colour range) plus the track's
// colour code points — the way ffmpeg builds it.
func vp9Entry(t *mkv.Track, firstFrame []byte) ([]byte, error) {
	record := vpcCRecord(t.CodecPrivate)
	if record == nil {
		h, err := parseVP9FrameHeader(firstFrame)
		if err != nil {
			return nil, errf("track %d (vp9): %w", t.ID, err)
		}
		fullRange := byte(0)
		if h.fullRange || (t.ColorRange != nil && *t.ColorRange == 2) {
			fullRange = 1
		}
		record = []byte{
			h.profile,
			vp9Level(t.Width, t.Height),
			h.bitDepth<<4 | h.chroma<<1 | fullRange,
			byte(cicp(t.ColorPrimaries)),
			byte(cicp(t.ColorTransfer)),
			byte(cicp(t.ColorSpace)),
			0, 0, // codecInitializationDataSize = 0
		}
	}
	config := fullBox("vpcC", 1, 0, func(w *bw) { w.bytes(record) })
	return visualSampleEntry("vp09", t, config), nil
}

// vp9Level returns the VP9 level code (10*major + minor, e.g. 21 = level 2.1)
// for a w*h picture, the smallest level whose MaxLumaPictureSize (VP9 spec
// Annex A) fits. A valid level is mandatory in the vpcC and the codec string:
// players reject level 0. Picture size is the dominant constraint; a
// frame-rate-based bump would only ever raise the level, so this conservative
// choice stays a valid, decodable declaration. nil dimensions default to
// level 1.0.
func vp9Level(w, h *uint32) byte {
	var size uint64
	if w != nil && h != nil {
		size = uint64(*w) * uint64(*h)
	}
	for _, e := range []struct {
		code   byte
		maxPic uint64
	}{
		{10, 36864}, {11, 73728}, {20, 122880}, {21, 245760},
		{30, 552960}, {31, 983040}, {40, 2228224}, {41, 2228224},
		{50, 8912896}, {51, 8912896}, {52, 8912896},
		{60, 35651584}, {61, 35651584},
	} {
		if size <= e.maxPic {
			return e.code
		}
	}
	return 62
}

// vp9RecordFromSampleEntry pulls the VPCodecConfigurationRecord out of a built
// vp09 sample entry (its vpcC FullBox child): [size][vpcC][version+flags(4)]
// [record]. Used for the codec string when the source has no CodecPrivate and
// vp9Entry derived the record from the first frame.
func vp9RecordFromSampleEntry(entry []byte) []byte {
	i := bytes.Index(entry, []byte("vpcC"))
	if i < 0 || i+8 > len(entry) {
		return nil
	}
	return entry[i+8:] // skip the fourcc and the FullBox version/flags
}

// vpcCRecord returns the VPCodecConfigurationRecord fields when the Matroska
// CodecPrivate already holds one (with or without the 4-byte FullBox prefix —
// both forms exist in the wild), nil otherwise.
func vpcCRecord(cp []byte) []byte {
	switch {
	case len(cp) >= 12 && cp[0] <= 1: // FullBox: version(1) flags(3) precede the record
		return cp[4:]
	case len(cp) >= 8:
		return cp
	}
	return nil
}

// vp9Header holds the colour-config fields of a VP9 keyframe's uncompressed
// header (the fields a vpcC needs).
type vp9Header struct {
	profile   byte
	bitDepth  byte
	chroma    byte // vpcC chromaSubsampling: 0/1 = 4:2:0, 2 = 4:2:2, 3 = 4:4:4
	fullRange bool
}

// parseVP9FrameHeader reads the start of a VP9 KEYFRAME's uncompressed header.
func parseVP9FrameHeader(b []byte) (*vp9Header, error) {
	r := &bitReader{data: b}
	if r.bits(2) != 2 {
		return nil, errf("VP9: bad frame_marker")
	}
	profile := byte(r.bits(1)) | byte(r.bits(1))<<1
	if profile == 3 {
		r.bits(1) // reserved_zero
	}
	if r.bits(1) == 1 {
		return nil, errf("VP9: show_existing_frame, not a keyframe")
	}
	frameType := r.bits(1)
	r.bits(2) // show_frame, error_resilient_mode
	if frameType != 0 {
		return nil, errf("VP9: first sample is not a keyframe")
	}
	if r.bits(24) != 0x498342 {
		return nil, errf("VP9: bad frame sync code")
	}
	h := &vp9Header{profile: profile, bitDepth: 8}
	if profile >= 2 {
		if r.bits(1) == 1 {
			h.bitDepth = 12
		} else {
			h.bitDepth = 10
		}
	}
	if colorSpace := r.bits(3); colorSpace != 7 { // 7 = CS_RGB
		h.fullRange = r.bits(1) == 1
		if profile == 1 || profile == 3 {
			sx, sy := r.bits(1), r.bits(1)
			r.bits(1) // reserved_zero
			switch {
			case sx == 1 && sy == 1:
				h.chroma = 0 // 4:2:0
			case sx == 1:
				h.chroma = 2 // 4:2:2
			default:
				h.chroma = 3 // 4:4:4
			}
		} else {
			h.chroma = 0 // profiles 0 and 2 are always 4:2:0
		}
	} else {
		h.fullRange = true
		h.chroma = 3 // CS_RGB is 4:4:4
	}
	if r.err {
		return nil, errf("VP9: truncated frame header")
	}
	return h, nil
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
		if pasp := paspBox(t); pasp != nil {
			w.bytes(pasp)
		}
		if dv := dvConfigBox(t); dv != nil {
			w.bytes(dv)
		}
	})
}

// paspBox builds a PixelAspectRatio box ('pasp') for an anamorphic track so the
// MP4 signals the same display aspect the source did. The pixel aspect ratio is
// recovered from the display and coded dimensions: hSpacing:vSpacing =
// (DisplayWidth · Height) : (DisplayHeight · Width). Returns nil for square
// pixels or when dimensions are missing.
func paspBox(t *mkv.Track) []byte {
	if t.Width == nil || t.Height == nil || *t.Width == 0 || *t.Height == 0 {
		return nil
	}
	if t.DisplayWidth == nil || t.DisplayHeight == nil || *t.DisplayWidth == 0 || *t.DisplayHeight == 0 {
		return nil
	}
	num := uint64(*t.DisplayWidth) * uint64(*t.Height)
	den := uint64(*t.DisplayHeight) * uint64(*t.Width)
	if num == 0 || den == 0 || num == den {
		return nil // square pixels — no pasp needed
	}
	g := gcdU64(num, den)
	return boxf("pasp", func(w *bw) {
		w.u32(uint32(num / g)) // hSpacing
		w.u32(uint32(den / g)) // vSpacing
	})
}

// gcdU64 returns the greatest common divisor of a and b (gcd(x,0)=x, never 0).
func gcdU64(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}

// dvConfigBox builds the Dolby Vision configuration box (dvcC or dvvC, per
// profile) for a track that carries a Dolby Vision configuration, so DV survives
// a remux into MP4. Returns nil when the track is not Dolby Vision.
func dvConfigBox(t *mkv.Track) []byte {
	if t.DolbyVision == nil {
		return nil
	}
	name, _ := t.DolbyVision.BoxType()
	return box(name, mkv.EncodeDolbyVisionConfig(t.DolbyVision))
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
// followed by its codec configuration box. A source track missing Channels or
// SampleRate (spec-invalid, unseen in real muxer output) falls back to 2 ch /
// 48000 Hz; without a real SampleRate the media timescale falls back to
// milliseconds, so the sample-exact edit-list guarantee degrades to ms
// precision for that track.
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
