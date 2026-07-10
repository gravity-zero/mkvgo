package mp4

// cenc_vp9.go - VP9 subsample-encryption split for Common Encryption
// (ISO/IEC 23001-7), following the VP Codec ISO Media File Format Binding.
//
// Clear/protected rule: within each VP9 frame, the uncompressed_header()
// (VP9 Bitstream & Decoding Process Specification, section 6.2) stays clear
// - a decoder/CDM must read profile, dimensions, loop filter, quantization
// and tile layout before it can touch any encrypted byte - and everything
// after it (the compressed header plus the tile data) is one continuous
// protected region. A sample may be a "superframe": several frames packed
// together with a trailing superframe index; the per-frame rule applies to
// each frame and the index itself is left clear (a decoder needs it, in the
// clear, to split the sample before it can even find the frames).
//
// A frame with show_existing_frame==1 carries no compressed data at all
// (it just repeats a previously decoded frame): the whole frame is clear.

import "encoding/binary"

// vp9Splitter is the stateful VP9 subsample splitter. It carries the last
// frame width across a segment's samples, so an inter frame that reuses a
// reference frame's dimensions (found_ref==1, the common case) resolves them
// from an earlier frame - within CENC each segment opens on a keyframe that
// sets the width, so the state is self-contained per segment. Resolution
// changes mid-segment (rare) still fall back to the last width.
type vp9Splitter struct{ lastWidth int }

func (s *vp9Splitter) split(sample []byte) ([]cencSubsample, error) {
	if len(sample) == 0 {
		return nil, errf("VP9 CENC: empty sample")
	}
	frames, indexLen, err := vp9SplitSuperframe(sample)
	if err != nil {
		return nil, err
	}
	subs := make([]cencSubsample, 0, len(frames)+1)
	for _, fr := range frames {
		sub, w, err := vp9FrameSubsample(sample[fr[0]:fr[1]], s.lastWidth)
		if err != nil {
			return nil, err
		}
		if w > 0 {
			s.lastWidth = w
		}
		subs = append(subs, sub)
	}
	if indexLen > 0 {
		subs = append(subs, vp9ClampSubsample(indexLen, indexLen))
	}
	return subs, nil
}

// splitVP9Subsamples returns the clear/protected subsample layout for one
// standalone VP9 coded sample (no cross-sample state): one cencSubsample per
// frame (clear = the frame's uncompressed header, protected = the rest), plus -
// for a superframe - a final clear-only subsample covering the trailing index.
// Segment encryption uses vp9Splitter instead, which threads the width.
func splitVP9Subsamples(sample []byte) ([]cencSubsample, error) {
	if len(sample) == 0 {
		return nil, errf("VP9 CENC: empty sample")
	}
	frames, indexLen, err := vp9SplitSuperframe(sample)
	if err != nil {
		return nil, err
	}
	subs := make([]cencSubsample, 0, len(frames)+1)
	knownWidth := 0
	for _, fr := range frames {
		sub, w, err := vp9FrameSubsample(sample[fr[0]:fr[1]], knownWidth)
		if err != nil {
			return nil, err
		}
		if w > 0 {
			knownWidth = w
		}
		subs = append(subs, sub)
	}
	if indexLen > 0 {
		subs = append(subs, vp9ClampSubsample(indexLen, indexLen))
	}
	return subs, nil
}

// vp9SplitSuperframe detects and parses a trailing VP9 superframe index
// (Annex B), returning each frame's [start,end) byte range within sample (in
// bitstream order) and the index's own byte length (0 when sample is a
// single frame with no index). The index is: a marker byte (superframe_marker
// f(3) == 0b110, bytes_per_framesize_minus_1 f(2), frames_in_superframe_minus_1
// f(3)), then frames_in_superframe * bytes_per_framesize bytes of
// little-endian frame sizes, then the same marker byte repeated - both
// copies must match for the trailing byte to actually be a superframe index
// rather than a coincidental frame byte.
func vp9SplitSuperframe(sample []byte) (frames [][2]int, indexLen int, err error) {
	marker := sample[len(sample)-1]
	if marker&0xE0 != 0xC0 {
		return [][2]int{{0, len(sample)}}, 0, nil
	}
	bytesPerSize := int((marker>>3)&3) + 1
	frameCount := int(marker&7) + 1
	indexLen = 2 + frameCount*bytesPerSize
	if indexLen > len(sample) || sample[len(sample)-indexLen] != marker {
		// The trailing byte only looks like a marker: treat the sample as a
		// single frame (real superframe indices always duplicate the marker
		// at both ends).
		return [][2]int{{0, len(sample)}}, 0, nil
	}
	body := sample[len(sample)-indexLen+1 : len(sample)-1]
	framesEnd := len(sample) - indexLen
	off := 0
	for i := 0; i < frameCount; i++ {
		var buf [4]byte
		copy(buf[:], body[i*bytesPerSize:(i+1)*bytesPerSize])
		sz := int(binary.LittleEndian.Uint32(buf[:]))
		if off+sz > framesEnd {
			return nil, 0, errf("VP9 CENC: superframe index frame size exceeds sample bounds")
		}
		frames = append(frames, [2]int{off, off + sz})
		off += sz
	}
	return frames, indexLen, nil
}

// vp9FrameSubsample splits one VP9 frame (already extracted from any
// superframe wrapping) into its clear (uncompressed header)/protected (rest)
// subsample. knownWidth is the frame width established by a previous frame
// in the same call (0 if none yet); it returns the width this frame
// establishes (0 if it does not - show_existing_frame, or an inter frame
// that reused knownWidth without changing it).
func vp9FrameSubsample(frame []byte, knownWidth int) (cencSubsample, int, error) {
	if len(frame) == 0 {
		return cencSubsample{}, 0, errf("VP9 CENC: empty frame in sample")
	}
	r := &bitReader{data: frame}
	if r.bits(2) != 2 {
		return cencSubsample{}, 0, errf("VP9 CENC: bad frame_marker")
	}
	profile := byte(r.bits(1)) | byte(r.bits(1))<<1
	if profile == 3 {
		r.bits(1) // reserved_zero
	}
	if r.bits(1) == 1 { // show_existing_frame: no compressed data at all
		if r.err {
			return cencSubsample{}, 0, errf("VP9 CENC: truncated frame header")
		}
		return vp9ClampSubsample(len(frame), len(frame)), 0, nil
	}
	frameType := r.bits(1)
	showFrame := r.bits(1)
	errorResilient := r.bits(1)

	var newWidth int
	if frameType == 0 { // KEY_FRAME
		if r.bits(24) != 0x498342 {
			return cencSubsample{}, 0, errf("VP9 CENC: bad frame sync code")
		}
		vp9ColorConfig(r, profile)
		newWidth = int(r.bits(16)) + 1
		r.bits(16) // frame_height_minus_1
		vp9RenderSize(r)
	} else {
		intraOnly := uint32(0)
		if showFrame == 0 {
			intraOnly = r.bits(1)
		}
		if errorResilient == 0 {
			r.bits(2) // reset_frame_context
		}
		if intraOnly == 1 {
			if r.bits(24) != 0x498342 {
				return cencSubsample{}, 0, errf("VP9 CENC: bad frame sync code")
			}
			if profile > 0 {
				vp9ColorConfig(r, profile)
			}
			r.bits(8) // refresh_frame_flags
			newWidth = int(r.bits(16)) + 1
			r.bits(16) // frame_height_minus_1
			vp9RenderSize(r)
		} else {
			r.bits(8) // refresh_frame_flags
			for i := 0; i < 3; i++ {
				r.bits(3) // ref_frame_idx
				r.bits(1) // ref_frame_sign_bias
			}
			w, err := vp9FrameSizeWithRefs(r, knownWidth)
			if err != nil {
				return cencSubsample{}, 0, err
			}
			newWidth = w
			r.bits(1) // allow_high_precision_mv
			vp9ReadInterpolationFilter(r)
		}
	}

	if errorResilient == 0 {
		r.bits(1) // refresh_frame_context
		r.bits(1) // frame_parallel_decoding_mode
	}
	r.bits(2) // frame_context_idx

	vp9LoopFilterParams(r)
	vp9QuantizationParams(r)
	vp9SegmentationParams(r)
	if err := vp9TileInfo(r, newWidth); err != nil {
		return cencSubsample{}, 0, err
	}
	r.bits(16) // header_size_in_bytes

	if r.err {
		return cencSubsample{}, 0, errf("VP9 CENC: truncated uncompressed header")
	}
	headerBytes := (int(r.pos) + 7) / 8
	return vp9ClampSubsample(headerBytes, len(frame)), newWidth, nil
}

// vp9ClampSubsample splits a region of length total into a clear prefix of
// clearLen bytes and a protected remainder, clamping clearLen to fit both
// total and a subsample's 16-bit clear field (ISO/IEC 23001-7); any excess
// spills into the protected count instead of overflowing the field.
func vp9ClampSubsample(clearLen, total int) cencSubsample {
	if clearLen > total {
		clearLen = total
	}
	if clearLen > 0xFFFF {
		clearLen = 0xFFFF
	}
	return cencSubsample{clear: uint16(clearLen), protected: uint32(total - clearLen)}
}

// vp9ColorConfig consumes color_config()'s bits (profile determines whether
// ten_or_twelve_bit and the chroma subsampling flags are present); the
// decoded values themselves are not needed here, only their bit width.
func vp9ColorConfig(r *bitReader, profile byte) {
	if profile >= 2 {
		r.bits(1) // ten_or_twelve_bit
	}
	const csRGB = 7
	colorSpace := r.bits(3)
	if colorSpace != csRGB {
		r.bits(1) // color_range
		if profile == 1 || profile == 3 {
			r.bits(2) // subsampling_x, subsampling_y
			r.bits(1) // reserved_zero
		}
	} else if profile == 1 || profile == 3 {
		r.bits(1) // reserved_zero
	}
}

// vp9RenderSize consumes render_size()'s bits.
func vp9RenderSize(r *bitReader) {
	if r.bits(1) == 1 { // render_and_frame_size_different
		r.bits(16) // render_width_minus_1
		r.bits(16) // render_height_minus_1
	}
}

// vp9FrameSizeWithRefs consumes frame_size_with_refs()'s bits and returns the
// resulting frame width. When the frame reuses a reference frame's stored
// size (found_ref==1, the common case for inter frames whose dimensions do
// not change), that size is decoder state this stateless, per-sample
// function cannot see on its own - knownWidth (the width established by an
// earlier frame within the SAME call, e.g. an earlier frame of the same
// superframe) is the only source available, and an error is returned when
// none exists. A caller processing a full track in order and wanting exact
// correctness across every sample would need to thread the last known width
// through successive calls itself.
func vp9FrameSizeWithRefs(r *bitReader, knownWidth int) (int, error) {
	foundRef := false
	for i := 0; i < 3; i++ {
		if r.bits(1) == 1 {
			foundRef = true
			break
		}
	}
	var width int
	if foundRef {
		if knownWidth <= 0 {
			return 0, errf("VP9 CENC: frame_size_with_refs reuses a reference frame's size, unresolvable from a standalone sample")
		}
		width = knownWidth
	} else {
		width = int(r.bits(16)) + 1
		r.bits(16) // frame_height_minus_1
	}
	vp9RenderSize(r)
	return width, nil
}

// vp9ReadInterpolationFilter consumes read_interpolation_filter()'s bits.
func vp9ReadInterpolationFilter(r *bitReader) {
	if r.bits(1) == 0 { // is_filter_switchable == 0
		r.bits(2) // raw_interpolation_filter
	}
}

// vp9SignedValue consumes an su(n) value: n magnitude bits plus one sign bit.
func vp9SignedValue(r *bitReader, n int) {
	r.bits(n)
	r.bits(1)
}

// vp9LoopFilterParams consumes loop_filter_params()'s bits.
func vp9LoopFilterParams(r *bitReader) {
	r.bits(6)           // loop_filter_level
	r.bits(3)           // loop_filter_sharpness
	if r.bits(1) == 1 { // loop_filter_delta_enabled
		if r.bits(1) == 1 { // loop_filter_delta_update
			for i := 0; i < 4; i++ {
				if r.bits(1) == 1 { // update_ref_delta
					vp9SignedValue(r, 6)
				}
			}
			for i := 0; i < 2; i++ {
				if r.bits(1) == 1 { // update_mode_delta
					vp9SignedValue(r, 6)
				}
			}
		}
	}
}

// vp9ReadDeltaQ consumes one read_delta_q()'s bits.
func vp9ReadDeltaQ(r *bitReader) {
	if r.bits(1) == 1 { // delta_coded
		vp9SignedValue(r, 4)
	}
}

// vp9QuantizationParams consumes quantization_params()'s bits.
func vp9QuantizationParams(r *bitReader) {
	r.bits(8)        // base_q_idx
	vp9ReadDeltaQ(r) // delta_q_y_dc
	vp9ReadDeltaQ(r) // delta_q_uv_dc
	vp9ReadDeltaQ(r) // delta_q_uv_ac
}

// vp9ReadProb consumes one read_prob()'s bits.
func vp9ReadProb(r *bitReader) {
	if r.bits(1) == 1 { // prob_coded
		r.bits(8)
	}
}

// vp9SegmentationParams consumes segmentation_params()'s bits.
func vp9SegmentationParams(r *bitReader) {
	if r.bits(1) == 0 { // segmentation_enabled
		return
	}
	if r.bits(1) == 1 { // segmentation_update_map
		for i := 0; i < 7; i++ {
			vp9ReadProb(r)
		}
		temporalUpdate := r.bits(1)
		for i := 0; i < 3; i++ {
			if temporalUpdate == 1 {
				vp9ReadProb(r)
			}
		}
	}
	if r.bits(1) == 1 { // segmentation_update_data
		r.bits(1) // segmentation_abs_or_delta_update
		featureBits := [4]int{8, 6, 2, 0}
		featureSigned := [4]bool{true, true, false, false}
		for i := 0; i < 8; i++ { // MAX_SEGMENTS
			for j := 0; j < 4; j++ { // SEG_LVL_MAX
				if r.bits(1) == 1 { // feature_enabled
					if featureBits[j] > 0 {
						r.bits(featureBits[j])
					}
					if featureSigned[j] {
						r.bits(1) // feature_sign
					}
				}
			}
		}
	}
}

// vp9TileInfo consumes tile_info()'s bits. It needs frameWidth (established
// earlier in uncompressed_header() by frame_size()/frame_size_with_refs())
// to compute Sb64Cols, the same value the encoder used to bound
// tile_cols_log2's range.
func vp9TileInfo(r *bitReader, frameWidth int) error {
	if frameWidth <= 0 {
		return errf("VP9 CENC: tile_info needs a known frame width")
	}
	miCols := (frameWidth + 7) >> 3
	sb64Cols := (miCols + 7) >> 3
	const maxTileWidthB64 = 64
	const minTileWidthB64 = 4
	minLog2 := 0
	for (maxTileWidthB64 << minLog2) < sb64Cols {
		minLog2++
	}
	maxLog2 := 1
	for (sb64Cols >> maxLog2) >= minTileWidthB64 {
		maxLog2++
	}
	maxLog2--
	tileColsLog2 := minLog2
	for tileColsLog2 < maxLog2 {
		if r.bits(1) == 1 { // increment_tile_cols_log2
			tileColsLog2++
		} else {
			break
		}
	}
	if r.bits(1) == 1 { // tile_rows_log2
		r.bits(1) // increment_tile_rows_log2
	}
	return nil
}
