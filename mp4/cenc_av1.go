package mp4

// cenc_av1.go - AV1 subsample split for Common Encryption (ISO/IEC 23001-7),
// per the AV1 Codec ISO Media File Format Binding, section 2.4 (Encryption).
//
// An AV1 sample (one temporal unit, exactly the layout av1C/ISOBMFF already
// stores) is a back-to-back sequence of OBUs in the "low overhead bitstream
// format": each OBU is obu_header (1 byte, +1 when obu_extension_flag is
// set), then - when obu_has_size_field is set - a leb128 obu_size, then
// obu_size bytes of payload. Only the last OBU of the sample may omit
// obu_has_size_field, in which case its payload runs to the end of the
// sample.
//
// Per-OBU-type rule this file implements:
//   - obu_header and the leb128 obu_size (when present) are ALWAYS clear,
//     for every OBU type.
//   - OBU_TILE_GROUP (4): coded tile data. This assumes the common
//     single-tile case (NumTiles == 1), where tile_group_header is 0 bits,
//     so the entire payload is protected tile data.
//   - OBU_FRAME (6): frame_header_obu() (AV1 spec 5.9) immediately followed,
//     byte-aligned, by tile_group_obu() (5.10, frame_obu()). This file parses
//     frame_header_obu() bit-accurately - using the sequence header active
//     for the segment (5.5.1, tracked by av1Splitter across the segment's
//     samples) plus, for inter frames, the dimensions/order hints of frames
//     already decoded earlier in the segment (frame_size_with_refs(),
//     skip_mode_params(); AV1 spec 7.20's "reference frame update process")
//     - to find frame_header_obu()'s bit length, rounds it up to the next
//     byte (byte_alignment()) and splits there: clear = obu_header + leb128
//     size + frame_header bytes, protected = the tile data that follows,
//     under the same single-tile/16-byte-alignment rule as OBU_TILE_GROUP.
//     Two constructs are deliberately NOT implemented and error instead of
//     guessing a split: show_existing_frame == 1 inside an OBU_FRAME (no
//     tile data would follow it at all - not something a real encoder
//     combining OBU_FRAME produces, and this file has no RefFrameType[] to
//     interpret it), and frame_refs_short_signaling == 1 (its ref_frame_idx[]
//     values come from set_frame_refs()'s order-hint search over stored
//     reference state, which this file does not replicate). An OBU_FRAME
//     with no sequence header parsed yet in the segment is also an error.
//   - every other OBU type (OBU_SEQUENCE_HEADER=1 - parsed for its fields,
//     see above, but its bytes stay clear like the rest -,
//     OBU_TEMPORAL_DELIMITER=2, OBU_FRAME_HEADER=3, OBU_METADATA=5,
//     OBU_REDUNDANT_FRAME_HEADER=7, OBU_TILE_LIST=8, OBU_PADDING=15, and any
//     reserved/unknown type) is entirely clear.
//
// 16-byte remainder rule: per the binding, a protected region should be a
// whole multiple of 16 bytes (an AES block), so any trailing remainder of an
// OBU_TILE_GROUP's (or an OBU_FRAME's tile data) payload (< 16 bytes) is left
// clear and folded into the clear run preceding the next subsample (or
// becomes a final trailing clear subsample if it is the last OBU).
//
// What still needs real-decoder validation before this is production-grade:
//   - The frame_header_obu() parse above has been checked bit-by-bit against
//     the AV1 specification and round-trips against synthetic fixtures in
//     this package's tests, but that only proves the split is internally
//     self-consistent (sum of clear+protected == sample length, the header
//     bytes are unchanged, the tile bytes are the ones encrypted) - not that
//     a real AV1 decoder/CDM accepts the resulting bitstream. That requires
//     actually decoding real encoder output split this way.
//   - The single-tile assumption for OBU_TILE_GROUP and OBU_FRAME's trailing
//     tile_group_obu(): multi-tile streams have a non-empty tile_group_header
//     (tile_start_and_end_present_flag, tg_start/tg_end when present), which
//     this file does not parse and would currently miscount as protected
//     tile data.
//   - OBU_FRAME_HEADER (a frame header with no combined tile data) is left
//     entirely clear and does not update av1Splitter's reference-slot state;
//     a segment that mixes OBU_FRAME_HEADER+OBU_TILE_GROUP frames with
//     combined OBU_FRAME frames would lose reference-slot updates from the
//     former, which could make a later OBU_FRAME's frame_size_with_refs() or
//     skip_mode_params() see stale (or, per the fail-loud check, missing)
//     reference state. Only combined OBU_FRAME was in scope for this file.

const (
	obuSeqHeader         = 1
	obuTemporalDelimiter = 2
	obuFrameHeader       = 3
	obuTileGroup         = 4
	obuMetadata          = 5
	obuFrame             = 6
	obuRedundantFrameHdr = 7
	obuTileList          = 8
	obuPadding           = 15
)

// readLEB128 decodes an AV1 unsigned little-endian base-128 varint (the
// spec's leb128() syntax) from the start of b: up to 8 groups of 7 bits,
// each group's high bit signaling whether another group follows. Returns the
// decoded value and the number of bytes consumed.
func readLEB128(b []byte) (value uint64, n int, err error) {
	for i := 0; i < 8; i++ {
		if i >= len(b) {
			return 0, 0, errf("cenc: av1: truncated leb128 in OBU size field")
		}
		v := b[i]
		value |= uint64(v&0x7f) << uint(i*7)
		n++
		if v&0x80 == 0 {
			return value, n, nil
		}
	}
	return 0, 0, errf("cenc: av1: leb128 exceeds the 8-byte maximum")
}

// av1RefFrameState is the subset of one NUM_REF_FRAMES reference-frame slot's
// stored state (AV1 spec 7.20, "reference frame update process") that a
// later frame's frame_size_with_refs() or skip_mode_params() needs: the
// slot's dimensions and order hint, as last set by a frame whose
// refresh_frame_flags included this slot. valid is false until some frame in
// the segment refreshes it (a CENC segment always opens on a keyframe, which
// refreshes every slot, so in practice this is only false before that first
// keyframe is parsed).
type av1RefFrameState struct {
	valid                                                 bool
	upscaledWidth, frameHeight, renderWidth, renderHeight int
	orderHint                                             uint32
}

// av1Splitter is the stateful AV1 subsample splitter: it carries the
// sequence header active for the segment (set on OBU_SEQUENCE_HEADER) and
// the per-reference-slot state combined OBU_FRAME parsing needs, both valid
// across the segment's samples (a CENC segment opens on a keyframe carrying
// the sequence header, so this state is self-contained per segment).
type av1Splitter struct {
	seq  *av1SeqHeader
	refs [av1NumRefFrames]av1RefFrameState
}

func (s *av1Splitter) split(sample []byte) ([]cencSubsample, error) {
	var subs []cencSubsample
	clearRun := 0
	i := 0
	for i < len(sample) {
		start := i
		header := sample[i]
		obuType := (header >> 3) & 0x0f
		extFlag := (header >> 2) & 1
		hasSize := (header >> 1) & 1
		i++
		var temporalID, spatialID uint32
		if extFlag == 1 {
			if i >= len(sample) {
				return nil, errf("cenc: av1: truncated OBU extension header")
			}
			ext := sample[i]
			temporalID = uint32(ext >> 5)
			spatialID = uint32((ext >> 3) & 3)
			i++
		}
		var payloadLen int
		if hasSize == 1 {
			v, n, err := readLEB128(sample[i:])
			if err != nil {
				return nil, err
			}
			i += n
			payloadLen = int(v)
		} else {
			// Only the last OBU of the sample may omit obu_size: its payload
			// then runs to the end of the sample.
			payloadLen = len(sample) - i
		}
		if payloadLen < 0 || i+payloadLen > len(sample) {
			return nil, errf("cenc: av1: OBU payload size %d exceeds sample bounds", payloadLen)
		}
		clearHeaderBytes := i - start
		payload := sample[i : i+payloadLen]

		switch obuType {
		case obuSeqHeader:
			seq, err := parseAV1SequenceHeader(payload)
			if err != nil {
				return nil, err
			}
			s.seq = seq
			clearRun += clearHeaderBytes + payloadLen
		case obuTileGroup:
			// Single-tile assumption (tile_group_header == 0 bits): the
			// whole payload is coded tile data, protected.
			clearRun += clearHeaderBytes
			if clearRun > 0xffff {
				return nil, errf("cenc: av1: clear region %d exceeds the 16-bit subsample limit", clearRun)
			}
			protectedLen := payloadLen / 16 * 16
			trailing := payloadLen - protectedLen
			subs = append(subs, cencSubsample{clear: uint16(clearRun), protected: uint32(protectedLen)})
			clearRun = trailing
		case obuFrame:
			if s.seq == nil {
				return nil, errf("cenc: av1: OBU_FRAME with no active sequence header in this segment")
			}
			r := &bitReader{data: payload}
			fh, err := parseAV1FrameHeader(r, s.seq, &s.refs, temporalID, spatialID)
			if err != nil {
				return nil, err
			}
			clearInFrame := (int(r.pos) + 7) / 8
			if clearInFrame > payloadLen {
				return nil, errf("cenc: av1: frame_header_obu parsed past the OBU_FRAME payload")
			}
			// Reference frame update process (AV1 spec 7.20): every slot
			// this frame refreshes now holds this frame's own dimensions and
			// order hint, for a later frame in the segment to reuse.
			for slot := 0; slot < av1NumRefFrames; slot++ {
				if fh.refreshFrameFlags&(1<<uint(slot)) != 0 {
					s.refs[slot] = av1RefFrameState{
						valid:         true,
						upscaledWidth: fh.upscaledWidth,
						frameHeight:   fh.frameHeight,
						renderWidth:   fh.renderWidth,
						renderHeight:  fh.renderHeight,
						orderHint:     fh.orderHint,
					}
				}
			}
			clearRun += clearHeaderBytes + clearInFrame
			if clearRun > 0xffff {
				return nil, errf("cenc: av1: clear region %d exceeds the 16-bit subsample limit", clearRun)
			}
			tileLen := payloadLen - clearInFrame
			protectedLen := tileLen / 16 * 16
			trailing := tileLen - protectedLen
			subs = append(subs, cencSubsample{clear: uint16(clearRun), protected: uint32(protectedLen)})
			clearRun = trailing
		case obuTemporalDelimiter, obuFrameHeader, obuMetadata, obuRedundantFrameHdr, obuTileList, obuPadding:
			clearRun += clearHeaderBytes + payloadLen
		default:
			// Reserved/unknown OBU type: no subsample rule applies in the
			// binding; treat as opaque and clear, like the named types above.
			clearRun += clearHeaderBytes + payloadLen
		}
		i += payloadLen
	}
	if clearRun > 0 || len(subs) == 0 {
		if clearRun > 0xffff {
			return nil, errf("cenc: av1: clear region %d exceeds the 16-bit subsample limit", clearRun)
		}
		subs = append(subs, cencSubsample{clear: uint16(clearRun), protected: 0})
	}
	return subs, nil
}

// splitAV1Subsamples parses one standalone AV1 coded sample (no cross-sample
// state) through a fresh av1Splitter. Segment encryption uses one av1Splitter
// per segment instead (newVideoSubsampleSplitter), so an OBU_FRAME can use
// the sequence header and reference state an earlier sample in the same
// segment established.
func splitAV1Subsamples(sample []byte) ([]cencSubsample, error) {
	return (&av1Splitter{}).split(sample)
}

// --- sequence_header_obu() (AV1 spec 5.5.1) ---------------------------------

const (
	av1SelectScreenContentTools = 2
	av1SelectIntegerMv          = 2
)

// av1SeqHeader is the subset of sequence_header_obu()'s fields that
// frame_header_obu() needs to parse a frame header following it in the same
// segment.
type av1SeqHeader struct {
	seqProfile                    uint32
	reducedStillPictureHeader     bool
	frameIDNumbersPresent         bool
	deltaFrameIDLengthMinus2      uint32
	additionalFrameIDLengthMinus1 uint32
	frameWidthBitsMinus1          uint32
	frameHeightBitsMinus1         uint32
	maxFrameWidthMinus1           uint32
	maxFrameHeightMinus1          uint32
	use128x128Superblock          bool
	enableOrderHint               bool
	orderHintBits                 uint32
	enableRefFrameMvs             bool
	seqForceScreenContentTools    uint32
	seqForceIntegerMv             uint32
	enableSuperres                bool
	enableCdef                    bool
	enableRestoration             bool
	enableWarpedMotion            bool

	monoChrome       bool
	numPlanes        int
	bitDepth         int
	subsamplingX     uint32
	subsamplingY     uint32
	separateUVDeltaQ bool

	decoderModelInfoPresent           bool
	equalPictureInterval              bool
	bufferDelayLengthMinus1           uint32
	bufferRemovalTimeLengthMinus1     uint32
	framePresentationTimeLengthMinus1 uint32

	operatingPointsCntMinus1 uint32
	operatingPointIdc        []uint32
	decoderModelPresentForOp []bool

	filmGrainParamsPresent bool
}

// parseAV1SequenceHeader parses sequence_header_obu()'s bits, keeping only
// the fields frame_header_obu() needs later. It is bit-accurate (reads
// exactly what a real decoder reads) but never needs the OBU's own byte
// length: any trailing padding within the OBU payload past the last parsed
// field is simply left unread.
func parseAV1SequenceHeader(payload []byte) (*av1SeqHeader, error) {
	r := &bitReader{data: payload}
	s := &av1SeqHeader{}

	s.seqProfile = r.bits(3)
	r.bits(1) // still_picture
	s.reducedStillPictureHeader = r.bits(1) == 1

	if s.reducedStillPictureHeader {
		r.bits(5) // seq_level_idx[0]
	} else {
		timingInfoPresent := r.bits(1) == 1
		if timingInfoPresent {
			r.bits(32) // num_units_in_display_tick
			r.bits(32) // time_scale
			s.equalPictureInterval = r.bits(1) == 1
			if s.equalPictureInterval {
				av1UVLC(r) // num_ticks_per_picture_minus_1
			}
			s.decoderModelInfoPresent = r.bits(1) == 1
			if s.decoderModelInfoPresent {
				s.bufferDelayLengthMinus1 = r.bits(5)
				r.bits(32) // num_units_in_decoding_tick
				s.bufferRemovalTimeLengthMinus1 = r.bits(5)
				s.framePresentationTimeLengthMinus1 = r.bits(5)
			}
		}
		initialDisplayDelayPresent := r.bits(1) == 1
		s.operatingPointsCntMinus1 = r.bits(5)
		n := int(s.operatingPointsCntMinus1) + 1
		s.operatingPointIdc = make([]uint32, n)
		s.decoderModelPresentForOp = make([]bool, n)
		for k := 0; k < n; k++ {
			s.operatingPointIdc[k] = r.bits(12)
			seqLevelIdx := r.bits(5)
			if seqLevelIdx > 7 {
				r.bits(1) // seq_tier[k]
			}
			if s.decoderModelInfoPresent {
				present := r.bits(1) == 1
				s.decoderModelPresentForOp[k] = present
				if present {
					n2 := int(s.bufferDelayLengthMinus1) + 1
					r.bits(n2) // decoder_buffer_delay
					r.bits(n2) // encoder_buffer_delay
					r.bits(1)  // low_delay_mode_flag
				}
			}
			if initialDisplayDelayPresent {
				if r.bits(1) == 1 { // initial_display_delay_present_for_this_op
					r.bits(4) // initial_display_delay_minus_1
				}
			}
		}
	}

	s.frameWidthBitsMinus1 = r.bits(4)
	s.frameHeightBitsMinus1 = r.bits(4)
	s.maxFrameWidthMinus1 = r.bits(int(s.frameWidthBitsMinus1) + 1)
	s.maxFrameHeightMinus1 = r.bits(int(s.frameHeightBitsMinus1) + 1)

	if !s.reducedStillPictureHeader {
		s.frameIDNumbersPresent = r.bits(1) == 1
	}
	if s.frameIDNumbersPresent {
		s.deltaFrameIDLengthMinus2 = r.bits(4)
		s.additionalFrameIDLengthMinus1 = r.bits(3)
	}

	s.use128x128Superblock = r.bits(1) == 1
	r.bits(1) // enable_filter_intra
	r.bits(1) // enable_intra_edge_filter

	if s.reducedStillPictureHeader {
		s.seqForceScreenContentTools = av1SelectScreenContentTools
		s.seqForceIntegerMv = av1SelectIntegerMv
		s.orderHintBits = 0
	} else {
		r.bits(1) // enable_interintra_compound
		r.bits(1) // enable_masked_compound
		s.enableWarpedMotion = r.bits(1) == 1
		r.bits(1) // enable_dual_filter
		s.enableOrderHint = r.bits(1) == 1
		if s.enableOrderHint {
			r.bits(1) // enable_jnt_comp
			s.enableRefFrameMvs = r.bits(1) == 1
		}
		if r.bits(1) == 1 { // seq_choose_screen_content_tools
			s.seqForceScreenContentTools = av1SelectScreenContentTools
		} else {
			s.seqForceScreenContentTools = r.bits(1)
		}
		if s.seqForceScreenContentTools > 0 {
			if r.bits(1) == 1 { // seq_choose_integer_mv
				s.seqForceIntegerMv = av1SelectIntegerMv
			} else {
				s.seqForceIntegerMv = r.bits(1)
			}
		} else {
			s.seqForceIntegerMv = av1SelectIntegerMv
		}
		if s.enableOrderHint {
			s.orderHintBits = r.bits(3) + 1
		}
	}

	s.enableSuperres = r.bits(1) == 1
	s.enableCdef = r.bits(1) == 1
	s.enableRestoration = r.bits(1) == 1

	// color_config()
	highBitdepth := r.bits(1) == 1
	switch {
	case s.seqProfile == 2 && highBitdepth:
		if r.bits(1) == 1 { // twelve_bit
			s.bitDepth = 12
		} else {
			s.bitDepth = 10
		}
	case s.seqProfile <= 2:
		if highBitdepth {
			s.bitDepth = 10
		} else {
			s.bitDepth = 8
		}
	}
	if s.seqProfile == 1 {
		s.monoChrome = false
	} else {
		s.monoChrome = r.bits(1) == 1
	}
	if s.monoChrome {
		s.numPlanes = 1
	} else {
		s.numPlanes = 3
	}
	colorDescPresent := r.bits(1) == 1
	colorPrimaries, transferCharacteristics, matrixCoefficients := uint32(2), uint32(2), uint32(2) // *_UNSPECIFIED
	if colorDescPresent {
		colorPrimaries = r.bits(8)
		transferCharacteristics = r.bits(8)
		matrixCoefficients = r.bits(8)
	}
	if s.monoChrome {
		r.bits(1) // color_range
		s.subsamplingX, s.subsamplingY = 1, 1
	} else if colorPrimaries == 1 && transferCharacteristics == 13 && matrixCoefficients == 0 {
		// CP_BT_709 / TC_SRGB / MC_IDENTITY: full-range 4:4:4, no bit read.
		s.subsamplingX, s.subsamplingY = 0, 0
	} else {
		r.bits(1) // color_range
		switch s.seqProfile {
		case 0:
			s.subsamplingX, s.subsamplingY = 1, 1
		case 1:
			s.subsamplingX, s.subsamplingY = 0, 0
		default:
			if s.bitDepth == 12 {
				s.subsamplingX = r.bits(1)
				if s.subsamplingX == 1 {
					s.subsamplingY = r.bits(1)
				} else {
					s.subsamplingY = 0
				}
			} else {
				s.subsamplingX, s.subsamplingY = 1, 0
			}
		}
		if s.subsamplingX == 1 && s.subsamplingY == 1 {
			r.bits(2) // chroma_sample_position
		}
	}
	if !s.monoChrome {
		s.separateUVDeltaQ = r.bits(1) == 1
	}

	s.filmGrainParamsPresent = r.bits(1) == 1

	if r.err {
		return nil, errf("cenc: av1: truncated sequence_header_obu")
	}
	return s, nil
}

// --- bit-level helpers shared by sequence/frame header parsing --------------

func av1FloorLog2(x uint32) int {
	s := 0
	for x != 0 {
		x >>= 1
		s++
	}
	return s - 1
}

// av1TileLog2 is tile_log2(blkSize, target): the smallest k with
// (blkSize << k) >= target.
func av1TileLog2(blkSize, target int) int {
	k := 0
	for (blkSize << uint(k)) < target {
		k++
	}
	return k
}

func av1Clip3(lo, hi, v int32) int32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// av1SU reads an su(n) value (AV1 spec 4.10.6): n bits, the top bit acting as
// a sign in a two's-complement-like range.
func av1SU(r *bitReader, n int) int32 {
	v := r.bits(n)
	signMask := uint32(1) << uint(n-1)
	if v&signMask != 0 {
		return int32(v) - int32(signMask)*2
	}
	return int32(v)
}

// av1NS reads an ns(n) non-symmetric value (AV1 spec 4.10.7): a value in
// [0, n-1] using the minimum number of bits, without requiring n to be a
// power of two.
func av1NS(r *bitReader, n int) uint32 {
	if n < 1 {
		return 0
	}
	w := av1FloorLog2(uint32(n)) + 1
	m := (uint32(1) << uint(w)) - uint32(n)
	v := r.bits(w - 1)
	if v < m {
		return v
	}
	extra := r.bits(1)
	return (v << 1) - m + extra
}

// av1UVLC reads a uvlc() value (AV1 spec 4.10.3).
func av1UVLC(r *bitReader) uint32 {
	leadingZeros := 0
	for r.bits(1) != 1 {
		leadingZeros++
		if r.err || leadingZeros >= 32 {
			break
		}
	}
	if leadingZeros >= 32 {
		return 0xFFFFFFFF
	}
	value := r.bits(leadingZeros)
	return value + (1 << uint(leadingZeros)) - 1
}

// av1DecodeSubexp reads decode_subexp(numSyms) (AV1 spec 7.11.5's inverse):
// a variable-length code over [0, numSyms-1]. Used by read_global_param()'s
// decode_signed_subexp_with_ref() - only the bits it reads matter here (see
// av1ReadGlobalParam), not the recentred value.
func av1DecodeSubexp(r *bitReader, numSyms int) uint32 {
	i := 0
	mk := 0
	const k = 3
	for {
		b2 := k
		if i > 0 {
			b2 = k + i - 1
		}
		a := 1 << uint(b2)
		if numSyms <= mk+3*a {
			v := av1NS(r, numSyms-mk)
			return v + uint32(mk)
		}
		if r.bits(1) == 1 { // subexp_more_bits
			i++
			mk += a
		} else {
			v := r.bits(b2) // subexp_bits
			return v + uint32(mk)
		}
	}
}

// --- frame_header_obu() (AV1 spec 5.9) --------------------------------------

const (
	av1NumRefFrames   = 8
	av1RefsPerFrame   = 7
	av1PrimaryRefNone = 7

	av1FrameKey       = 0
	av1FrameInter     = 1
	av1FrameIntraOnly = 2
	av1FrameSwitch    = 3

	av1MaxSegments = 8
	av1SegLvlMax   = 8
	av1SegLvlAltQ  = 0

	av1MaxTileWidth = 4096
	av1MaxTileArea  = 4096 * 2304
	av1MaxTileCols  = 64
	av1MaxTileRows  = 64

	av1GmIdentity    = 0
	av1GmTranslation = 1
	av1GmRotZoom     = 2
	av1GmAffine      = 3

	av1GmAbsAlphaBits     = 12
	av1GmAbsTransOnlyBits = 9
	av1GmAbsTransBits     = 12
)

var av1SegFeatureBits = [av1SegLvlMax]int{8, 6, 6, 6, 6, 3, 0, 0}
var av1SegFeatureSigned = [av1SegLvlMax]bool{true, true, true, true, true, false, false, false}
var av1SegFeatureMax = [av1SegLvlMax]int32{255, 63, 63, 63, 63, 7, 0, 0}

// av1FrameHeader holds the uncompressed_header() fields later syntax
// elements or this file's caller (av1Splitter) need, for one OBU_FRAME.
type av1FrameHeader struct {
	frameType               uint32
	frameIsIntra            bool
	showFrame               bool
	showableFrame           bool
	errorResilientMode      bool
	allowScreenContentTools bool
	forceIntegerMv          bool
	frameSizeOverrideFlag   bool
	orderHint               uint32
	primaryRefFrame         uint32
	refreshFrameFlags       uint32
	refFrameIdx             [av1RefsPerFrame]int

	frameWidth, frameHeight   int
	upscaledWidth             int
	renderWidth, renderHeight int
	miCols, miRows            int
	allowIntrabc              bool
	allowHighPrecisionMv      bool
	useRefFrameMvs            bool

	baseQIdx                                              uint32
	deltaQYDc, deltaQUDc, deltaQUAc, deltaQVDc, deltaQVAc int32
	usingQmatrix                                          bool

	segmentationEnabled bool
	featureEnabled      [av1MaxSegments][av1SegLvlMax]bool
	featureData         [av1MaxSegments][av1SegLvlMax]int32

	deltaQPresent bool

	codedLossless bool
	allLossless   bool

	tileColsLog2, tileRowsLog2 int

	referenceSelect bool
}

// parseAV1FrameHeader parses uncompressed_header() through tile_info()'s end
// (AV1 spec 5.9.2, following section 5.9's field order), returning the
// resulting header (the caller rounds r.pos up to the next byte for
// byte_alignment() and splits there). seq is the sequence header active for
// the segment; refs is the per-slot reference-frame state established by
// earlier frames in the segment (frame_size_with_refs(), skip_mode_params());
// temporalID/spatialID come from this OBU's extension header (0 if absent).
func parseAV1FrameHeader(r *bitReader, seq *av1SeqHeader, refs *[av1NumRefFrames]av1RefFrameState, temporalID, spatialID uint32) (*av1FrameHeader, error) {
	fh := &av1FrameHeader{}
	const allFrames = (1 << av1NumRefFrames) - 1

	if seq.reducedStillPictureHeader {
		fh.frameType = av1FrameKey
		fh.frameIsIntra = true
		fh.showFrame = true
		fh.showableFrame = false
	} else {
		if r.bits(1) == 1 { // show_existing_frame
			return nil, errf("cenc: av1: combined OBU_FRAME with show_existing_frame is not supported (no tile data follows; encode as OBU_FRAME_HEADER instead)")
		}
		fh.frameType = r.bits(2)
		fh.frameIsIntra = fh.frameType == av1FrameIntraOnly || fh.frameType == av1FrameKey
		fh.showFrame = r.bits(1) == 1
		if fh.showFrame && seq.decoderModelInfoPresent && !seq.equalPictureInterval {
			r.bits(int(seq.framePresentationTimeLengthMinus1) + 1) // temporal_point_info()
		}
		if fh.showFrame {
			fh.showableFrame = fh.frameType != av1FrameKey
		} else {
			fh.showableFrame = r.bits(1) == 1
		}
		if fh.frameType == av1FrameSwitch || (fh.frameType == av1FrameKey && fh.showFrame) {
			fh.errorResilientMode = true
		} else {
			fh.errorResilientMode = r.bits(1) == 1
		}
	}

	r.bits(1) // disable_cdf_update
	if seq.seqForceScreenContentTools == av1SelectScreenContentTools {
		fh.allowScreenContentTools = r.bits(1) == 1
	} else {
		fh.allowScreenContentTools = seq.seqForceScreenContentTools != 0
	}
	if fh.allowScreenContentTools {
		if seq.seqForceIntegerMv == av1SelectIntegerMv {
			fh.forceIntegerMv = r.bits(1) == 1
		} else {
			fh.forceIntegerMv = seq.seqForceIntegerMv != 0
		}
	}
	if fh.frameIsIntra {
		fh.forceIntegerMv = true
	}

	if seq.frameIDNumbersPresent {
		idLen := int(seq.additionalFrameIDLengthMinus1) + int(seq.deltaFrameIDLengthMinus2) + 3
		r.bits(idLen) // current_frame_id
	}

	switch {
	case fh.frameType == av1FrameSwitch:
		fh.frameSizeOverrideFlag = true
	case seq.reducedStillPictureHeader:
		fh.frameSizeOverrideFlag = false
	default:
		fh.frameSizeOverrideFlag = r.bits(1) == 1
	}

	fh.orderHint = r.bits(int(seq.orderHintBits))

	if fh.frameIsIntra || fh.errorResilientMode {
		fh.primaryRefFrame = av1PrimaryRefNone
	} else {
		fh.primaryRefFrame = r.bits(3)
	}

	if seq.decoderModelInfoPresent {
		if r.bits(1) == 1 { // buffer_removal_time_present_flag
			n := int(seq.operatingPointsCntMinus1) + 1
			for op := 0; op < n; op++ {
				if seq.decoderModelPresentForOp[op] {
					opPtIdc := seq.operatingPointIdc[op]
					inTemporal := (opPtIdc>>temporalID)&1 != 0
					inSpatial := (opPtIdc>>(spatialID+8))&1 != 0
					if opPtIdc == 0 || (inTemporal && inSpatial) {
						r.bits(int(seq.bufferRemovalTimeLengthMinus1) + 1) // buffer_removal_time[op]
					}
				}
			}
		}
	}

	if fh.frameType == av1FrameSwitch || (fh.frameType == av1FrameKey && fh.showFrame) {
		fh.refreshFrameFlags = allFrames
	} else {
		fh.refreshFrameFlags = r.bits(8)
	}

	if !fh.frameIsIntra || fh.refreshFrameFlags != allFrames {
		if fh.errorResilientMode && seq.enableOrderHint {
			for i := 0; i < av1NumRefFrames; i++ {
				r.bits(int(seq.orderHintBits)) // ref_order_hint[i]
			}
		}
	}

	if fh.frameIsIntra {
		av1FrameSize(r, seq, fh)
		av1RenderSize(r, fh)
		if fh.allowScreenContentTools && fh.upscaledWidth == fh.frameWidth {
			fh.allowIntrabc = r.bits(1) == 1
		}
	} else {
		if seq.enableOrderHint {
			if r.bits(1) == 1 { // frame_refs_short_signaling
				return nil, errf("cenc: av1: frame_refs_short_signaling (set_frame_refs) is not supported for subsample encryption")
			}
		}
		for i := 0; i < av1RefsPerFrame; i++ {
			fh.refFrameIdx[i] = int(r.bits(3))
			if seq.frameIDNumbersPresent {
				r.bits(int(seq.deltaFrameIDLengthMinus2) + 2) // delta_frame_id_minus_1
			}
		}
		if fh.frameSizeOverrideFlag && !fh.errorResilientMode {
			if err := av1FrameSizeWithRefs(r, seq, fh, refs); err != nil {
				return nil, err
			}
		} else {
			av1FrameSize(r, seq, fh)
			av1RenderSize(r, fh)
		}
		if fh.forceIntegerMv {
			fh.allowHighPrecisionMv = false
		} else {
			fh.allowHighPrecisionMv = r.bits(1) == 1
		}
		if r.bits(1) == 0 { // is_filter_switchable == 0
			r.bits(2) // raw interpolation_filter
		}
		r.bits(1) // is_motion_mode_switchable
		if fh.errorResilientMode || !seq.enableRefFrameMvs {
			fh.useRefFrameMvs = false
		} else {
			fh.useRefFrameMvs = r.bits(1) == 1
		}
	}

	if !seq.reducedStillPictureHeader {
		r.bits(1) // disable_frame_end_update_cdf
	}

	av1TileInfo(r, seq, fh)
	av1QuantizationParams(r, seq, fh)
	if err := av1SegmentationParams(r, fh); err != nil {
		return nil, err
	}
	av1DeltaQParams(r, fh)
	av1DeltaLfParams(r, fh)

	fh.codedLossless = true
	for segID := 0; segID < av1MaxSegments; segID++ {
		qindex := av1GetQIndex(fh, segID)
		lossless := qindex == 0 && fh.deltaQYDc == 0 && fh.deltaQUAc == 0 &&
			fh.deltaQUDc == 0 && fh.deltaQVAc == 0 && fh.deltaQVDc == 0
		if !lossless {
			fh.codedLossless = false
		}
	}
	fh.allLossless = fh.codedLossless && fh.frameWidth == fh.upscaledWidth

	av1LoopFilterParams(r, seq, fh)
	av1CdefParams(r, seq, fh)
	av1LrParams(r, seq, fh)

	if !fh.codedLossless { // read_tx_mode()
		r.bits(1) // tx_mode_select
	}

	if !fh.frameIsIntra { // frame_reference_mode()
		fh.referenceSelect = r.bits(1) == 1
	}

	av1SkipModeParams(r, seq, fh, refs)

	if !fh.frameIsIntra && !fh.errorResilientMode && seq.enableWarpedMotion {
		r.bits(1) // allow_warped_motion
	}

	r.bits(1) // reduced_tx_set

	av1GlobalMotionParams(r, fh)
	av1FilmGrainParams(r, seq, fh)

	if r.err {
		return nil, errf("cenc: av1: truncated frame_header_obu")
	}
	return fh, nil
}

// av1FrameSize is frame_size() (AV1 spec 5.9.5).
func av1FrameSize(r *bitReader, seq *av1SeqHeader, fh *av1FrameHeader) {
	if fh.frameSizeOverrideFlag {
		fh.frameWidth = int(r.bits(int(seq.frameWidthBitsMinus1)+1)) + 1
		fh.frameHeight = int(r.bits(int(seq.frameHeightBitsMinus1)+1)) + 1
	} else {
		fh.frameWidth = int(seq.maxFrameWidthMinus1) + 1
		fh.frameHeight = int(seq.maxFrameHeightMinus1) + 1
	}
	av1SuperresParams(r, seq, fh)
	av1ComputeImageSize(fh)
}

// av1SuperresParams is superres_params() (AV1 spec 5.9.7).
func av1SuperresParams(r *bitReader, seq *av1SeqHeader, fh *av1FrameHeader) {
	const superresNum = 8
	const superresDenomMin = 9
	const superresDenomBits = 3
	useSuperres := false
	if seq.enableSuperres {
		useSuperres = r.bits(1) == 1
	}
	denom := superresNum
	if useSuperres {
		denom = int(r.bits(superresDenomBits)) + superresDenomMin
	}
	fh.upscaledWidth = fh.frameWidth
	fh.frameWidth = (fh.upscaledWidth*superresNum + denom/2) / denom
}

// av1ComputeImageSize is compute_image_size() (AV1 spec 5.9.6).
func av1ComputeImageSize(fh *av1FrameHeader) {
	fh.miCols = 2 * ((fh.frameWidth + 7) >> 3)
	fh.miRows = 2 * ((fh.frameHeight + 7) >> 3)
}

// av1RenderSize is render_size() (AV1 spec 5.9.8).
func av1RenderSize(r *bitReader, fh *av1FrameHeader) {
	if r.bits(1) == 1 { // render_and_frame_size_different
		fh.renderWidth = int(r.bits(16)) + 1
		fh.renderHeight = int(r.bits(16)) + 1
	} else {
		fh.renderWidth = fh.upscaledWidth
		fh.renderHeight = fh.frameHeight
	}
}

// av1FrameSizeWithRefs is frame_size_with_refs() (AV1 spec 5.9.9): an inter
// frame may reuse an already-decoded reference frame's stored dimensions
// (found_ref == 1) instead of signaling its own.
func av1FrameSizeWithRefs(r *bitReader, seq *av1SeqHeader, fh *av1FrameHeader, refs *[av1NumRefFrames]av1RefFrameState) error {
	foundRef := false
	for i := 0; i < av1RefsPerFrame; i++ {
		if r.bits(1) == 1 { // found_ref
			foundRef = true
			idx := fh.refFrameIdx[i]
			ref := refs[idx]
			if !ref.valid {
				return errf("cenc: av1: frame_size_with_refs reuses reference slot %d, which has no recorded size yet in this segment", idx)
			}
			fh.upscaledWidth = ref.upscaledWidth
			fh.frameWidth = ref.upscaledWidth
			fh.frameHeight = ref.frameHeight
			fh.renderWidth = ref.renderWidth
			fh.renderHeight = ref.renderHeight
			break
		}
	}
	if !foundRef {
		av1FrameSize(r, seq, fh)
		av1RenderSize(r, fh)
	} else {
		av1SuperresParams(r, seq, fh)
		av1ComputeImageSize(fh)
	}
	return nil
}

// av1TileInfo is tile_info() (AV1 spec 5.9.15): only the bits it reads
// matter here (locating quantization_params() and beyond), not the tile grid
// itself - the split still assumes a single tile, same as OBU_TILE_GROUP.
func av1TileInfo(r *bitReader, seq *av1SeqHeader, fh *av1FrameHeader) {
	sbShift := 4
	if seq.use128x128Superblock {
		sbShift = 5
	}
	sbSize := sbShift + 2
	sbCols := (fh.miCols + (1<<uint(sbShift) - 1)) >> uint(sbShift)
	sbRows := (fh.miRows + (1<<uint(sbShift) - 1)) >> uint(sbShift)
	maxTileWidthSb := av1MaxTileWidth >> sbSize
	maxTileAreaSb := av1MaxTileArea >> uint(2*sbSize)
	minLog2TileCols := av1TileLog2(maxTileWidthSb, sbCols)
	maxLog2TileCols := av1TileLog2(1, min(sbCols, av1MaxTileCols))
	maxLog2TileRows := av1TileLog2(1, min(sbRows, av1MaxTileRows))
	minLog2Tiles := max(minLog2TileCols, av1TileLog2(maxTileAreaSb, sbRows*sbCols))

	var tileColsLog2, tileRowsLog2 int

	if r.bits(1) == 1 { // uniform_tile_spacing_flag
		tileColsLog2 = minLog2TileCols
		for tileColsLog2 < maxLog2TileCols {
			if r.bits(1) == 1 { // increment_tile_cols_log2
				tileColsLog2++
			} else {
				break
			}
		}
		minLog2TileRows := max(minLog2Tiles-tileColsLog2, 0)
		tileRowsLog2 = minLog2TileRows
		for tileRowsLog2 < maxLog2TileRows {
			if r.bits(1) == 1 { // increment_tile_rows_log2
				tileRowsLog2++
			} else {
				break
			}
		}
	} else {
		startSb := 0
		tileCols := 0
		widestTileSb := 0
		for startSb < sbCols {
			maxWidth := min(sbCols-startSb, maxTileWidthSb)
			sizeSb := int(av1NS(r, maxWidth)) + 1 // width_in_sbs_minus_1 + 1
			if sizeSb > widestTileSb {
				widestTileSb = sizeSb
			}
			startSb += sizeSb
			tileCols++
		}
		tileColsLog2 = av1TileLog2(1, tileCols)

		if minLog2Tiles > 0 {
			maxTileAreaSb = (sbRows * sbCols) >> uint(minLog2Tiles+1)
		} else {
			maxTileAreaSb = sbRows * sbCols
		}
		maxTileHeightSb := 1
		if widestTileSb > 0 {
			maxTileHeightSb = max(maxTileAreaSb/widestTileSb, 1)
		}
		startSb = 0
		tileRows := 0
		for startSb < sbRows {
			maxHeight := min(sbRows-startSb, maxTileHeightSb)
			sizeSb := int(av1NS(r, maxHeight)) + 1 // height_in_sbs_minus_1 + 1
			startSb += sizeSb
			tileRows++
		}
		tileRowsLog2 = av1TileLog2(1, tileRows)
	}

	fh.tileColsLog2, fh.tileRowsLog2 = tileColsLog2, tileRowsLog2
	if tileColsLog2 > 0 || tileRowsLog2 > 0 {
		r.bits(tileRowsLog2 + tileColsLog2) // context_update_tile_id
		r.bits(2)                           // tile_size_bytes_minus_1
	}
}

// av1ReadDeltaQ is read_delta_q() (AV1 spec 5.9.12).
func av1ReadDeltaQ(r *bitReader) int32 {
	if r.bits(1) == 1 { // delta_coded
		return av1SU(r, 1+6)
	}
	return 0
}

// av1QuantizationParams is quantization_params() (AV1 spec 5.9.12).
func av1QuantizationParams(r *bitReader, seq *av1SeqHeader, fh *av1FrameHeader) {
	fh.baseQIdx = r.bits(8)
	fh.deltaQYDc = av1ReadDeltaQ(r)
	if seq.numPlanes > 1 {
		diffUVDelta := false
		if seq.separateUVDeltaQ {
			diffUVDelta = r.bits(1) == 1
		}
		fh.deltaQUDc = av1ReadDeltaQ(r)
		fh.deltaQUAc = av1ReadDeltaQ(r)
		if diffUVDelta {
			fh.deltaQVDc = av1ReadDeltaQ(r)
			fh.deltaQVAc = av1ReadDeltaQ(r)
		} else {
			fh.deltaQVDc = fh.deltaQUDc
			fh.deltaQVAc = fh.deltaQUAc
		}
	}
	fh.usingQmatrix = r.bits(1) == 1
	if fh.usingQmatrix {
		r.bits(4) // qm_y
		r.bits(4) // qm_u
		if seq.separateUVDeltaQ {
			r.bits(4) // qm_v
		}
	}
}

// av1SegmentationParams is segmentation_params() (AV1 spec 5.9.14). A frame
// that reuses a previous frame's feature data verbatim (primary_ref_frame
// set and segmentation_update_data == 0) is not supported: this file has no
// cross-frame store of FeatureEnabled/FeatureData, only of the
// reference-slot dimensions/order-hints frame_size_with_refs()/
// skip_mode_params() need.
func av1SegmentationParams(r *bitReader, fh *av1FrameHeader) error {
	fh.segmentationEnabled = r.bits(1) == 1
	if !fh.segmentationEnabled {
		return nil
	}
	updateData := true
	if fh.primaryRefFrame != av1PrimaryRefNone {
		if r.bits(1) == 1 { // segmentation_update_map
			r.bits(1) // segmentation_temporal_update
		}
		updateData = r.bits(1) == 1
	}
	if !updateData {
		return errf("cenc: av1: segmentation_params reusing a previous frame's feature data is not supported for subsample encryption")
	}
	for i := 0; i < av1MaxSegments; i++ {
		for j := 0; j < av1SegLvlMax; j++ {
			enabled := r.bits(1) == 1
			fh.featureEnabled[i][j] = enabled
			var clipped int32
			if enabled {
				bits := av1SegFeatureBits[j]
				limit := av1SegFeatureMax[j]
				if av1SegFeatureSigned[j] {
					clipped = av1Clip3(-limit, limit, av1SU(r, 1+bits))
				} else {
					clipped = av1Clip3(0, limit, int32(r.bits(bits)))
				}
			}
			fh.featureData[i][j] = clipped
		}
	}
	return nil
}

// av1DeltaQParams is delta_q_params() (AV1 spec 5.9.17).
func av1DeltaQParams(r *bitReader, fh *av1FrameHeader) {
	if fh.baseQIdx > 0 {
		fh.deltaQPresent = r.bits(1) == 1
	}
	if fh.deltaQPresent {
		r.bits(2) // delta_q_res
	}
}

// av1DeltaLfParams is delta_lf_params() (AV1 spec 5.9.18).
func av1DeltaLfParams(r *bitReader, fh *av1FrameHeader) {
	if !fh.deltaQPresent {
		return
	}
	deltaLfPresent := false
	if !fh.allowIntrabc {
		deltaLfPresent = r.bits(1) == 1
	}
	if deltaLfPresent {
		r.bits(2) // delta_lf_res
		r.bits(1) // delta_lf_multi
	}
}

// av1GetQIndex is get_qindex(1, segmentId) (AV1 spec 7.12.2) - ignoreDeltaQ
// is always 1 here since this file only needs it for the CodedLossless check
// in uncompressed_header(), which always calls it that way.
func av1GetQIndex(fh *av1FrameHeader, segmentID int) int32 {
	if fh.segmentationEnabled && fh.featureEnabled[segmentID][av1SegLvlAltQ] {
		q := int32(fh.baseQIdx) + fh.featureData[segmentID][av1SegLvlAltQ]
		return av1Clip3(0, 255, q)
	}
	return int32(fh.baseQIdx)
}

// av1LoopFilterParams is loop_filter_params() (AV1 spec 5.9.11).
func av1LoopFilterParams(r *bitReader, seq *av1SeqHeader, fh *av1FrameHeader) {
	if fh.codedLossless || fh.allowIntrabc {
		return
	}
	level0 := r.bits(6)
	level1 := r.bits(6)
	if seq.numPlanes > 1 && (level0 != 0 || level1 != 0) {
		r.bits(6) // loop_filter_level[2]
		r.bits(6) // loop_filter_level[3]
	}
	r.bits(3)           // loop_filter_sharpness
	if r.bits(1) == 1 { // loop_filter_delta_enabled
		if r.bits(1) == 1 { // loop_filter_delta_update
			const totalRefsPerFrame = 8
			for i := 0; i < totalRefsPerFrame; i++ {
				if r.bits(1) == 1 { // update_ref_delta
					av1SU(r, 1+6) // loop_filter_ref_deltas[i]
				}
			}
			for i := 0; i < 2; i++ {
				if r.bits(1) == 1 { // update_mode_delta
					av1SU(r, 1+6) // loop_filter_mode_deltas[i]
				}
			}
		}
	}
}

// av1CdefParams is cdef_params() (AV1 spec 5.9.19).
func av1CdefParams(r *bitReader, seq *av1SeqHeader, fh *av1FrameHeader) {
	if fh.codedLossless || fh.allowIntrabc || !seq.enableCdef {
		return
	}
	r.bits(2)           // cdef_damping_minus_3
	n := 1 << r.bits(2) // cdef_bits
	for i := 0; i < n; i++ {
		r.bits(4) // cdef_y_pri_strength
		r.bits(2) // cdef_y_sec_strength
		if seq.numPlanes > 1 {
			r.bits(4) // cdef_uv_pri_strength
			r.bits(2) // cdef_uv_sec_strength
		}
	}
}

// av1LrParams is lr_params() (AV1 spec 5.9.20).
func av1LrParams(r *bitReader, seq *av1SeqHeader, fh *av1FrameHeader) {
	if fh.allLossless || fh.allowIntrabc || !seq.enableRestoration {
		return
	}
	usesLr := false
	usesChromaLr := false
	for i := 0; i < seq.numPlanes; i++ {
		lrType := r.bits(2) // lr_type; raw 0 == RESTORE_NONE under Remap_Lr_Type
		if lrType != 0 {
			usesLr = true
			if i > 0 {
				usesChromaLr = true
			}
		}
	}
	if usesLr {
		if seq.use128x128Superblock {
			r.bits(1) // lr_unit_shift
		} else if r.bits(1) == 1 { // lr_unit_shift
			r.bits(1) // lr_unit_extra_shift
		}
		if seq.subsamplingX == 1 && seq.subsamplingY == 1 && usesChromaLr {
			r.bits(1) // lr_uv_shift
		}
	}
}

// av1RelativeDist is get_relative_dist() (AV1 spec 5.9.3).
func av1RelativeDist(seq *av1SeqHeader, a, b uint32) int32 {
	if !seq.enableOrderHint {
		return 0
	}
	diff := int32(a) - int32(b)
	m := int32(1) << uint(seq.orderHintBits-1)
	diff = (diff & (m - 1)) - (diff & m)
	return diff
}

// av1SkipModeParams is skip_mode_params() (AV1 spec 5.9.22): whether
// skip_mode_present is even coded depends on a forward/backward reference
// search over the reference slots' stored order hints.
func av1SkipModeParams(r *bitReader, seq *av1SeqHeader, fh *av1FrameHeader, refs *[av1NumRefFrames]av1RefFrameState) {
	skipModeAllowed := false
	if !fh.frameIsIntra && fh.referenceSelect && seq.enableOrderHint {
		forwardIdx, backwardIdx := -1, -1
		var forwardHint, backwardHint uint32
		for i := 0; i < av1RefsPerFrame; i++ {
			refHint := refs[fh.refFrameIdx[i]].orderHint
			dist := av1RelativeDist(seq, refHint, fh.orderHint)
			switch {
			case dist < 0:
				if forwardIdx < 0 || av1RelativeDist(seq, refHint, forwardHint) > 0 {
					forwardIdx = i
					forwardHint = refHint
				}
			case dist > 0:
				if backwardIdx < 0 || av1RelativeDist(seq, refHint, backwardHint) < 0 {
					backwardIdx = i
					backwardHint = refHint
				}
			}
		}
		if forwardIdx >= 0 {
			if backwardIdx >= 0 {
				skipModeAllowed = true
			} else {
				secondForwardIdx := -1
				var secondForwardHint uint32
				for i := 0; i < av1RefsPerFrame; i++ {
					refHint := refs[fh.refFrameIdx[i]].orderHint
					if av1RelativeDist(seq, refHint, forwardHint) < 0 {
						if secondForwardIdx < 0 || av1RelativeDist(seq, refHint, secondForwardHint) > 0 {
							secondForwardIdx = i
							secondForwardHint = refHint
						}
					}
				}
				skipModeAllowed = secondForwardIdx >= 0
			}
		}
	}
	if skipModeAllowed {
		r.bits(1) // skip_mode_present
	}
}

// av1ReadGlobalParam consumes one read_global_param() call's bits (AV1 spec
// 5.9.24, decode_signed_subexp_with_ref()). Only the number of bits read
// (bounded by mx, which depends only on idx/gmType/allow_high_precision_mv)
// matters here; the decoded value's recentring also needs PrevGmParams (the
// primary reference frame's own global motion - cross-frame decoder state
// this file does not track), but that value is only used AFTER
// decode_subexp's bits are already consumed, so it never changes how many
// bits are read - safe to disregard for locating the tile data boundary.
func av1ReadGlobalParam(r *bitReader, gmType int, fh *av1FrameHeader, idx int) {
	absBits := av1GmAbsAlphaBits
	if idx < 2 {
		if gmType == av1GmTranslation {
			absBits = av1GmAbsTransOnlyBits
			if !fh.allowHighPrecisionMv {
				absBits--
			}
		} else {
			absBits = av1GmAbsTransBits
		}
	}
	mx := 1 << uint(absBits)
	av1DecodeSubexp(r, 2*mx+1)
}

// av1GlobalMotionParams is global_motion_params() (AV1 spec 5.9.23).
func av1GlobalMotionParams(r *bitReader, fh *av1FrameHeader) {
	if fh.frameIsIntra {
		return
	}
	const (
		lastFrame   = 1
		altRefFrame = 7
	)
	for ref := lastFrame; ref <= altRefFrame; ref++ {
		gmType := av1GmIdentity
		if r.bits(1) == 1 { // is_global
			if r.bits(1) == 1 { // is_rot_zoom
				gmType = av1GmRotZoom
			} else if r.bits(1) == 1 { // is_translation
				gmType = av1GmTranslation
			} else {
				gmType = av1GmAffine
			}
		}
		if gmType >= av1GmRotZoom {
			av1ReadGlobalParam(r, gmType, fh, 2)
			av1ReadGlobalParam(r, gmType, fh, 3)
			if gmType == av1GmAffine {
				av1ReadGlobalParam(r, gmType, fh, 4)
				av1ReadGlobalParam(r, gmType, fh, 5)
			}
		}
		if gmType >= av1GmTranslation {
			av1ReadGlobalParam(r, gmType, fh, 0)
			av1ReadGlobalParam(r, gmType, fh, 1)
		}
	}
}

// av1FilmGrainParams is film_grain_params() (AV1 spec 5.9.30).
func av1FilmGrainParams(r *bitReader, seq *av1SeqHeader, fh *av1FrameHeader) {
	if !seq.filmGrainParamsPresent || (!fh.showFrame && !fh.showableFrame) {
		return
	}
	if r.bits(1) == 0 { // apply_grain
		return
	}
	r.bits(16) // grain_seed
	updateGrain := true
	if fh.frameType == av1FrameInter {
		updateGrain = r.bits(1) == 1
	}
	if !updateGrain {
		r.bits(3) // film_grain_params_ref_idx
		return
	}
	numYPoints := r.bits(4)
	for i := uint32(0); i < numYPoints; i++ {
		r.bits(8) // point_y_value
		r.bits(8) // point_y_scaling
	}
	chromaScalingFromLuma := false
	if !seq.monoChrome {
		chromaScalingFromLuma = r.bits(1) == 1
	}
	var numCbPoints, numCrPoints uint32
	implicitNoChromaPoints := seq.seqProfile == 0 && seq.subsamplingX == 1 && seq.subsamplingY == 1 && numYPoints == 0
	if !seq.monoChrome && !chromaScalingFromLuma && !implicitNoChromaPoints {
		numCbPoints = r.bits(4)
		for i := uint32(0); i < numCbPoints; i++ {
			r.bits(8)
			r.bits(8)
		}
		numCrPoints = r.bits(4)
		for i := uint32(0); i < numCrPoints; i++ {
			r.bits(8)
			r.bits(8)
		}
	}
	r.bits(2) // grain_scaling_minus_8
	arCoeffLag := r.bits(2)
	numPosLuma := 2 * arCoeffLag * (arCoeffLag + 1)
	numPosChroma := numPosLuma
	if numYPoints > 0 {
		numPosChroma = numPosLuma + 1
		for i := uint32(0); i < numPosLuma; i++ {
			r.bits(8) // ar_coeffs_y_plus_128
		}
	}
	if chromaScalingFromLuma || numCbPoints > 0 {
		for i := uint32(0); i < numPosChroma; i++ {
			r.bits(8) // ar_coeffs_cb_plus_128
		}
	}
	if chromaScalingFromLuma || numCrPoints > 0 {
		for i := uint32(0); i < numPosChroma; i++ {
			r.bits(8) // ar_coeffs_cr_plus_128
		}
	}
	r.bits(2) // ar_coeff_shift_minus_6
	r.bits(2) // grain_scale_shift
	if numCbPoints > 0 {
		r.bits(8) // cb_mult
		r.bits(8) // cb_luma_mult
		r.bits(9) // cb_offset
	}
	if numCrPoints > 0 {
		r.bits(8) // cr_mult
		r.bits(8) // cr_luma_mult
		r.bits(9) // cr_offset
	}
	r.bits(1) // overlap_flag
	r.bits(1) // clip_to_restricted_range
}
