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
//   - OBU_FRAME (6): frame_header_obu() followed by tile_group_header() and
//     tile data - but locating that boundary needs a full parse of the AV1
//     uncompressed frame header, which needs sequence-header state this
//     function does not track (frame_size_override_flag, superblock size,
//     reference-frame signaling, etc.). Guessing a split would either
//     encrypt live frame_header bits (corrupting every decoder, not just an
//     unauthorized one) or leave real tile data unencrypted while claiming
//     it is protected. This version instead treats the WHOLE OBU_FRAME
//     payload as clear: a conservative, decoder-safe fallback, not full
//     encryption of OBU_FRAME content. Content that must be encrypted
//     should be produced with frame_header and tile data split into
//     separate OBU_FRAME_HEADER + OBU_TILE_GROUP OBUs instead (both handled
//     exactly by the rules above), which is standard practice for encoders
//     that target CENC.
//   - every other OBU type (OBU_SEQUENCE_HEADER=1, OBU_TEMPORAL_DELIMITER=2,
//     OBU_FRAME_HEADER=3, OBU_METADATA=5, OBU_REDUNDANT_FRAME_HEADER=7,
//     OBU_TILE_LIST=8, OBU_PADDING=15, and any reserved/unknown type) is
//     entirely clear.
//
// 16-byte remainder rule: per the binding, a protected region should be a
// whole multiple of 16 bytes (an AES block), so any trailing remainder of an
// OBU_TILE_GROUP's payload (< 16 bytes) is left clear and folded into the
// clear run preceding the next subsample (or becomes a final trailing clear
// subsample if it is the last OBU).
//
// What still needs real-decoder validation before this is production-grade:
//   - The OBU_FRAME case above is a safe fallback only; it does not encrypt
//     combined-OBU_FRAME content. A real frame_header_obu() parser (with
//     sequence-header context threaded through) is needed to encrypt that
//     case, and any implementation of it must be validated by actually
//     decoding the resulting ciphertext/cleartext split with a real AV1
//     decoder, not just by round-tripping the encryption in this package.
//   - The single-tile assumption for OBU_TILE_GROUP: multi-tile streams
//     have a non-empty tile_group_header (tile_start_and_end_present_flag,
//     tg_start/tg_end when present), which this function does not parse and
//     would currently miscount as protected tile data.

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

// splitAV1Subsamples parses one AV1 coded sample (a temporal unit) into its
// CENC clear/protected subsample layout. See this file's doc comment for the
// exact per-OBU-type rule, the OBU_FRAME caveat and the 16-byte remainder
// handling.
func splitAV1Subsamples(sample []byte) ([]cencSubsample, error) {
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
		if extFlag == 1 {
			if i >= len(sample) {
				return nil, errf("cenc: av1: truncated OBU extension header")
			}
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

		switch obuType {
		case obuTileGroup:
			// Single-tile assumption (tile_group_header == 0 bits): the
			// whole payload is coded tile data, protected.
			clearRun += clearHeaderBytes
			protectedLen := payloadLen / 16 * 16
			trailing := payloadLen - protectedLen
			subs = append(subs, cencSubsample{clear: uint16(clearRun), protected: uint32(protectedLen)})
			clearRun = trailing
		case obuFrame:
			// Conservative fallback: see this file's doc comment.
			clearRun += clearHeaderBytes + payloadLen
		case obuSeqHeader, obuTemporalDelimiter, obuFrameHeader, obuMetadata, obuRedundantFrameHdr, obuTileList, obuPadding:
			clearRun += clearHeaderBytes + payloadLen
		default:
			// Reserved/unknown OBU type: no subsample rule applies in the
			// binding; treat as opaque and clear, like the named types above.
			clearRun += clearHeaderBytes + payloadLen
		}
		i += payloadLen
	}
	if clearRun > 0 || len(subs) == 0 {
		subs = append(subs, cencSubsample{clear: uint16(clearRun), protected: 0})
	}
	return subs, nil
}
