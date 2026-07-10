package mp4

// cenc_av1_frame_test.go - round-trip coverage for the combined OBU_FRAME
// subsample split (cenc_av1.go): frame_header_obu() parsed against a
// preceding sequence_header_obu(), clear prefix = obu_header + leb128 size +
// frame_header (byte-aligned), protected = the trailing tile data.
//
// NOTE: this only proves the split is internally self-consistent (byte
// accounting, header bytes untouched, tile bytes are the ones encrypted) -
// it does not prove a real AV1 decoder/CDM accepts the resulting bitstream.
// That needs decoding real encoder output split this way, which is out of
// scope for this package's own test suite.

import (
	"bytes"
	"crypto/aes"
	"testing"
)

// av1MinimalSequenceHeaderPayload builds a valid, minimal sequence_header_obu()
// payload (AV1 spec 5.5.1): reduced_still_picture_header=1 (forces every
// frame in the segment to KEY_FRAME/intra, sidestepping the inter-frame
// reference-signaling fields), a 64x64 max frame size, 4:2:0 8-bit color, no
// superres/cdef/restoration/film-grain - the smallest sequence header
// parseAV1SequenceHeader fully understands.
func av1MinimalSequenceHeaderPayload() []byte {
	var w bitWriter
	w.write(0, 3)  // seq_profile
	w.write(1, 1)  // still_picture
	w.write(1, 1)  // reduced_still_picture_header
	w.write(0, 5)  // seq_level_idx[0]
	w.write(5, 4)  // frame_width_bits_minus_1 (n=6)
	w.write(5, 4)  // frame_height_bits_minus_1 (n=6)
	w.write(63, 6) // max_frame_width_minus_1 (width 64)
	w.write(63, 6) // max_frame_height_minus_1 (height 64)
	w.write(0, 1)  // use_128x128_superblock
	w.write(0, 1)  // enable_filter_intra
	w.write(0, 1)  // enable_intra_edge_filter
	w.write(0, 1)  // enable_superres
	w.write(0, 1)  // enable_cdef
	w.write(0, 1)  // enable_restoration
	w.write(0, 1)  // high_bitdepth
	w.write(0, 1)  // mono_chrome
	w.write(0, 1)  // color_description_present_flag
	w.write(0, 1)  // color_range
	w.write(0, 2)  // chroma_sample_position
	w.write(0, 1)  // separate_uv_delta_q
	w.write(0, 1)  // film_grain_params_present
	return w.bytes()
}

// av1MinimalFrameHeaderBits builds the frame_header_obu() bitstream that
// matches av1MinimalSequenceHeaderPayload(): a lossless (base_q_idx=0, all
// deltas 0) key frame, one tile, no segmentation - the shortest header that
// exercises the CodedLossless/AllLossless short-circuits in
// loop_filter_params/cdef_params/lr_params/read_tx_mode. It consumes 18
// bits; byte_alignment() pads that to the 3 bytes this fixture returns.
func av1MinimalFrameHeaderBits() []byte {
	var w bitWriter
	w.write(0, 1) // disable_cdf_update
	w.write(0, 1) // allow_screen_content_tools
	w.write(0, 1) // render_and_frame_size_different
	w.write(1, 1) // tile_info: uniform_tile_spacing_flag
	w.write(0, 8) // base_q_idx
	w.write(0, 1) // delta_q_y_dc: delta_coded
	w.write(0, 1) // delta_q_u_dc: delta_coded
	w.write(0, 1) // delta_q_u_ac: delta_coded
	w.write(0, 1) // using_qmatrix
	w.write(0, 1) // segmentation_enabled
	w.write(0, 1) // reduced_tx_set
	return w.bytes()
}

// av1FrameOBUWithHeader wraps a minimal frame_header (matching
// av1MinimalSequenceHeaderPayload) followed by tileData into one OBU_FRAME.
func av1FrameOBUWithHeader(tileData []byte) []byte {
	payload := append(append([]byte(nil), av1MinimalFrameHeaderBits()...), tileData...)
	return obuLenPrefixed(obuFrame, payload)
}

func TestAV1CENCFrameOBURoundTrip(t *testing.T) {
	tileData := make([]byte, 50) // 50 % 16 = 2 -> exercises the trailing-remainder rule
	for i := range tileData {
		tileData[i] = byte(0x40 + i)
	}
	frameHeaderLen := len(av1MinimalFrameHeaderBits())

	var sample []byte
	sample = append(sample, av1TemporalDelimiter()...)
	sample = append(sample, obuLenPrefixed(obuSeqHeader, av1MinimalSequenceHeaderPayload())...)
	frameOBU := av1FrameOBUWithHeader(tileData)
	sample = append(sample, frameOBU...)

	splitter := &av1Splitter{}
	subs, err := splitter.split(sample)
	if err != nil {
		t.Fatalf("split: %v", err)
	}

	var total int
	for _, s := range subs {
		total += int(s.clear) + int(s.protected)
	}
	if total != len(sample) {
		t.Fatalf("subsample sizes sum to %d, want %d (len(sample))", total, len(sample))
	}

	key := []byte("0123456789abcdef")
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	counterBlock := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

	original := append([]byte(nil), sample...)
	working := append([]byte(nil), sample...)

	cencCTREncrypt(block, counterBlock, working, subs)
	if bytes.Equal(working, original) {
		t.Fatalf("encryption did not change any byte of the sample")
	}

	// The frame_header prefix (obu_header + leb128 size + frame_header bits,
	// byte-aligned) must be entirely clear and unchanged.
	frameOBUStart := len(sample) - len(frameOBU)
	const frameOBUHeaderLen = 2 // obu_header(1) + single-byte leb128 size(1), per obuLenPrefixed
	clearPrefixEnd := frameOBUStart + frameOBUHeaderLen + frameHeaderLen
	if !bytes.Equal(working[frameOBUStart:clearPrefixEnd], original[frameOBUStart:clearPrefixEnd]) {
		t.Fatalf("frame_header prefix changed after encryption:\n got  %x\n want %x",
			working[frameOBUStart:clearPrefixEnd], original[frameOBUStart:clearPrefixEnd])
	}

	// The tile data (50 bytes: 48 protected, a multiple of 16, plus a 2-byte
	// clear remainder) must be protected accordingly.
	const protectedLen = 48
	protectedStart := clearPrefixEnd
	if bytes.Equal(working[protectedStart:protectedStart+protectedLen], original[protectedStart:protectedStart+protectedLen]) {
		t.Fatalf("tile data's protected region was not encrypted")
	}
	remainderStart := protectedStart + protectedLen
	if !bytes.Equal(working[remainderStart:], original[remainderStart:]) {
		t.Fatalf("trailing < 16-byte tile remainder was encrypted (must stay clear):\n got  %x\n want %x",
			working[remainderStart:], original[remainderStart:])
	}
	for _, s := range subs {
		if s.protected%16 != 0 {
			t.Fatalf("protected subsample length %d is not a multiple of 16", s.protected)
		}
	}

	// CTR is symmetric: encrypting the ciphertext again with the same
	// counter recovers the plaintext.
	cencCTREncrypt(block, counterBlock, working, subs)
	if !bytes.Equal(working, original) {
		t.Fatalf("round-trip mismatch:\n got  %x\n want %x", working, original)
	}
}

// TestAV1CENCFrameOBUNoSequenceHeaderErrors locks in the fail-loud rule: an
// OBU_FRAME cannot be split without the sequence header active for the
// segment (frame_header_obu() needs it), so it must error rather than guess.
func TestAV1CENCFrameOBUNoSequenceHeaderErrors(t *testing.T) {
	sample := append(av1TemporalDelimiter(), av1FrameOBUWithHeader(make([]byte, 32))...)
	splitter := &av1Splitter{}
	if _, err := splitter.split(sample); err == nil {
		t.Fatal("OBU_FRAME with no active sequence header must error, not silently split")
	}
}
