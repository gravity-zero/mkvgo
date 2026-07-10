package mp4

// cenc_vp9_test.go - VP9 CENC subsample split round-trip and invariants: a
// synthetic VP9 sample (a spec-valid uncompressed header, and a superframe of
// two frames) proves the subsample offsets are self-consistent (sum of every
// subsample's clear+protected equals len(sample)) and that AES-CTR
// encryption over the derived subsamples touches only the protected bytes.

import (
	"bytes"
	"crypto/aes"
	"testing"
)

// vp9KeyframeHeader builds a spec-valid VP9 profile-0 keyframe uncompressed
// header for a width x height frame, sized so tile_info() needs no
// tile_cols_log2 bits (true for any width small enough that Sb64Cols fits in
// a single 64-superblock tile column) - exactly 14 bytes (112 bits),
// byte-aligned with no padding, which is what this fixture relies on.
func vp9KeyframeHeader(width, height uint32) []byte {
	var w bitWriter
	w.write(2, 2)         // frame_marker
	w.write(0, 1)         // profile_low_bit
	w.write(0, 1)         // profile_high_bit (profile 0)
	w.write(0, 1)         // show_existing_frame
	w.write(0, 1)         // frame_type (KEY_FRAME)
	w.write(1, 1)         // show_frame
	w.write(0, 1)         // error_resilient_mode
	w.write(0x498342, 24) // frame_sync_code
	w.write(2, 3)         // color_space (not CS_RGB)
	w.write(0, 1)         // color_range
	w.write(width-1, 16)  // frame_width_minus_1
	w.write(height-1, 16) // frame_height_minus_1
	w.write(0, 1)         // render_and_frame_size_different
	w.write(0, 1)         // refresh_frame_context
	w.write(0, 1)         // frame_parallel_decoding_mode
	w.write(0, 2)         // frame_context_idx
	w.write(0, 6)         // loop_filter_level
	w.write(0, 3)         // loop_filter_sharpness
	w.write(0, 1)         // loop_filter_delta_enabled
	w.write(0, 8)         // base_q_idx
	w.write(0, 1)         // delta_q_y_dc coded
	w.write(0, 1)         // delta_q_uv_dc coded
	w.write(0, 1)         // delta_q_uv_ac coded
	w.write(0, 1)         // segmentation_enabled
	w.write(0, 1)         // tile_rows_log2
	w.write(10, 16)       // header_size_in_bytes (placeholder value)
	return w.bytes()
}

// vp9SyntheticFrame builds one keyframe: a spec-valid header followed by
// tileLen bytes of "tile data" - a pattern seeded from fill and distinct from
// the header, so the round-trip test can tell the two regions apart.
func vp9SyntheticFrame(tileLen int, fill byte) []byte {
	frame := vp9KeyframeHeader(64, 64)
	if len(frame) != 14 {
		panic("vp9KeyframeHeader: unexpected header length, test fixture needs updating")
	}
	tile := make([]byte, tileLen)
	for i := range tile {
		tile[i] = fill + byte(i)
	}
	return append(frame, tile...)
}

// TestSplitVP9SubsamplesRoundTrip builds a single-frame VP9 sample, splits
// it, AES-CTR encrypts it over the derived subsamples, and decrypts (CTR is
// symmetric: encrypting the ciphertext again with the same counter recovers
// the plaintext) - proving the offsets are self-consistent and exactly cover
// the sample.
func TestSplitVP9SubsamplesRoundTrip(t *testing.T) {
	sample := vp9SyntheticFrame(40, 0x80)
	subs, err := splitVP9Subsamples(sample)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("subs = %+v, want exactly 1 (single frame, no superframe)", subs)
	}
	if subs[0].clear != 14 {
		t.Errorf("clear = %d, want 14 (the uncompressed header length)", subs[0].clear)
	}
	if subs[0].protected != 40 {
		t.Errorf("protected = %d, want 40 (the tile data)", subs[0].protected)
	}
	var total int
	for _, s := range subs {
		total += int(s.clear) + int(s.protected)
	}
	if total != len(sample) {
		t.Fatalf("sum of clear+protected = %d, want %d (len(sample))", total, len(sample))
	}

	block, err := aes.NewCipher([]byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	counterBlock := make([]byte, aes.BlockSize)
	counterBlock[aes.BlockSize-1] = 7

	original := append([]byte(nil), sample...)
	encrypted := append([]byte(nil), sample...)
	cencCTREncrypt(block, append([]byte(nil), counterBlock...), encrypted, subs)

	// Invariant: the clear (header) region is untouched, the protected
	// (tile) region changed.
	if !bytes.Equal(original[:14], encrypted[:14]) {
		t.Errorf("uncompressed header changed after encryption: before % x, after % x", original[:14], encrypted[:14])
	}
	if bytes.Equal(original[14:], encrypted[14:]) {
		t.Errorf("tile data unchanged after encryption (protected region was not touched)")
	}

	decrypted := append([]byte(nil), encrypted...)
	cencCTREncrypt(block, append([]byte(nil), counterBlock...), decrypted, subs)
	if !bytes.Equal(decrypted, original) {
		t.Fatalf("round trip mismatch:\n original  = % x\n decrypted = % x", original, decrypted)
	}
}

// TestSplitVP9SubsamplesSuperframe builds a 2-frame superframe and checks it
// yields 3 subsamples (2 frames + the trailing index), with the clear count
// of the index subsample exactly its byte length and the whole layout
// summing to len(sample).
func TestSplitVP9SubsamplesSuperframe(t *testing.T) {
	frame1 := vp9SyntheticFrame(20, 0x10)
	frame2 := vp9SyntheticFrame(30, 0x40)
	if len(frame1) > 255 || len(frame2) > 255 {
		t.Fatal("test fixture frames must fit a 1-byte superframe size field")
	}
	index := []byte{0xC1, byte(len(frame1)), byte(len(frame2)), 0xC1}
	sample := append(append(append([]byte{}, frame1...), frame2...), index...)

	subs, err := splitVP9Subsamples(sample)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 3 {
		t.Fatalf("subs = %+v, want 3 (2 frames + superframe index)", subs)
	}
	if subs[0].clear != 14 || subs[0].protected != 20 {
		t.Errorf("frame 1 subsample = %+v, want clear=14 protected=20", subs[0])
	}
	if subs[1].clear != 14 || subs[1].protected != 30 {
		t.Errorf("frame 2 subsample = %+v, want clear=14 protected=30", subs[1])
	}
	if subs[2].clear != uint16(len(index)) || subs[2].protected != 0 {
		t.Errorf("superframe index subsample = %+v, want clear=%d protected=0", subs[2], len(index))
	}
	var total int
	for _, s := range subs {
		total += int(s.clear) + int(s.protected)
	}
	if total != len(sample) {
		t.Fatalf("sum of clear+protected = %d, want %d (len(sample))", total, len(sample))
	}
}
