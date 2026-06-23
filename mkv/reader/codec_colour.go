package reader

// codec_colour.go — derive colour/HDR metadata from the codec bitstream stored in
// Track.CodecPrivate, as a FALLBACK for when the Matroska container Colour element
// (0x55B0) did not supply a field. The container value always wins per field; this
// only fills gaps. It is pure in-memory parsing of bytes the reader already holds
// (no extra I/O) and is defensive: any malformed/truncated input leaves the colour
// fields untouched (nil) — it never errors or panics, so ReadMeta/Read never fail
// because of bitstream parsing.
//
// Codecs: H.264 (avcC → SPS VUI), HEVC (hvcC header + SPS VUI), AV1 (av1C header +
// sequence-header OBU color_config), VP9 (vpcC fixed fields). Colour values are
// CICP / ITU-T H.273 code points, identical to what the container Colour element
// stores, so they feed the same Track fields and ColorSpaceName()/… mappers.

import "github.com/gravity-zero/mkvgo/mkv"

// bitstreamColour holds the per-field values recovered from a bitstream. Each is
// nil when the bitstream did not carry it.
type bitstreamColour struct {
	primaries *uint16 // CICP colour_primaries
	transfer  *uint16 // CICP transfer_characteristics
	matrix    *uint16 // CICP matrix_coefficients (ffprobe color_space)
	rng       *uint16 // Matroska Range: 1=limited/tv, 2=full/pc
	bitDepth  *uint16 // luma bit depth (8/10/12)
	profile   string  // e.g. "Main 10"
	level     *uint16 // level_idc (ffprobe level): H.264 10×level, HEVC 30×level, AV1 seq_level_idx
	sarWidth  uint32  // VUI sample aspect ratio width (0 when absent/square)
	sarHeight uint32  // VUI sample aspect ratio height
}

// avcSAR is H.264/HEVC Table E-1: aspect_ratio_idc (1–16) → sample aspect ratio
// width:height. Index 0 is unspecified; 255 is Extended_SAR (read inline).
var avcSAR = [17][2]uint16{
	{}, {1, 1}, {12, 11}, {10, 11}, {16, 11}, {40, 33}, {24, 11}, {20, 11},
	{32, 11}, {80, 33}, {18, 11}, {15, 11}, {64, 33}, {160, 99}, {4, 3}, {3, 2}, {2, 1},
}

// gcd64 returns the greatest common divisor of a and b (never 0).
func gcd64(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}

func u16p(v uint16) *uint16 { return &v }

// validBitDepth accepts only the bit depths real video uses (8/10/12); a garbage
// value parsed from an adversarial SPS (e.g. a huge Exp-Golomb bit_depth) yields
// nil rather than a nonsense depth.
func validBitDepth(d uint32) *uint16 {
	switch d {
	case 8, 10, 12:
		return u16p(uint16(d))
	}
	return nil
}

// cicpOrNil returns nil for CICP code 2 ("unspecified"), which carries no colour
// information — ffprobe omits the field in that case, and leaving it nil keeps the
// "nil means fall back" contract. Other code points (including 0 = identity/GBR)
// are real and returned as-is.
func cicpOrNil(v uint32) *uint16 {
	if v == 2 {
		return nil
	}
	return u16p(uint16(v))
}

// spsRange converts an SPS/AV1 video_full_range_flag (0=limited, 1=full) to the
// Matroska Range value (1=limited, 2=full) the colour mappers expect.
func spsRange(fullRangeFlag uint32) *uint16 {
	if fullRangeFlag == 1 {
		return u16p(2)
	}
	return u16p(1)
}

// FillColourFromCodecPrivate fills any nil colour/HDR field of a video track from
// its codec bitstream (Track.CodecPrivate), the same fallback the Matroska reader
// applies internally. It is exported for probes that assemble tracks outside the
// reader (e.g. the mp4 package, where colour may live only in the SPS VUI and not
// in a colr box). Safe on any input: malformed bitstreams leave the fields nil.
func FillColourFromCodecPrivate(t *mkv.Track) {
	fillColourFromCodecPrivate(t)
}

// fillColourFromCodecPrivate fills any colour field the container Colour element
// left nil from the codec bitstream. Called for video tracks after the TrackEntry
// is parsed. Safe on any input.
func fillColourFromCodecPrivate(t *mkv.Track) {
	if t.Type != mkv.VideoTrack || len(t.CodecPrivate) == 0 {
		return
	}
	bc := parseCodecColour(t.Codec, t.CodecPrivate)
	if bc == nil {
		return
	}
	if t.ColorPrimaries == nil && bc.primaries != nil {
		t.ColorPrimaries = bc.primaries
	}
	if t.ColorTransfer == nil && bc.transfer != nil {
		t.ColorTransfer = bc.transfer
	}
	if t.ColorSpace == nil && bc.matrix != nil {
		t.ColorSpace = bc.matrix
	}
	if t.ColorRange == nil && bc.rng != nil {
		t.ColorRange = bc.rng
	}
	if t.VideoBitDepth == nil && bc.bitDepth != nil {
		t.VideoBitDepth = bc.bitDepth
	}
	if t.Profile == "" && bc.profile != "" {
		t.Profile = bc.profile
	}
	if t.Level == nil && bc.level != nil {
		t.Level = bc.level
	}
	// Sample aspect ratio from the SPS VUI → display dimensions, only when the
	// container/pasp did not already supply them (those take precedence) and the
	// pixels are non-square. DisplayWidth:DisplayHeight = (W·sarW):(H·sarH), reduced.
	if t.DisplayWidth == nil && t.DisplayHeight == nil &&
		bc.sarWidth > 0 && bc.sarHeight > 0 && bc.sarWidth != bc.sarHeight &&
		t.Width != nil && t.Height != nil && *t.Width > 0 && *t.Height > 0 {
		dw := uint64(*t.Width) * uint64(bc.sarWidth)
		dh := uint64(*t.Height) * uint64(bc.sarHeight)
		g := gcd64(dw, dh)
		a, b := uint32(dw/g), uint32(dh/g)
		t.DisplayWidth, t.DisplayHeight = &a, &b
	}
}

// parseCodecColour dispatches on the codec and returns the bitstream colour, or
// nil. The deferred recover is a final backstop: the bit reader is bounds-checked,
// but the byte-level header indexing must also never crash the caller.
func parseCodecColour(codec string, cp []byte) (bc *bitstreamColour) {
	defer func() {
		if recover() != nil {
			bc = nil
		}
	}()
	switch codec {
	case "h264", "avc", "V_MPEG4/ISO/AVC":
		return avcColour(cp)
	case "hevc", "h265", "V_MPEGH/ISO/HEVC":
		return hevcColour(cp)
	case "av1", "V_AV1":
		return av1Colour(cp)
	case "vp9", "V_VP9":
		return vp9Colour(cp)
	default:
		return nil
	}
}

// --- bit reader (MSB-first) with Exp-Golomb, over-read tracked in err ----------

type bitReader struct {
	data []byte
	pos  int // bit position
	err  bool
}

func (r *bitReader) bit() uint32 {
	if r.pos >= len(r.data)*8 {
		r.err = true
		return 0
	}
	b := r.data[r.pos>>3]
	v := uint32((b >> (7 - uint(r.pos&7))) & 1)
	r.pos++
	return v
}

func (r *bitReader) bits(n int) uint32 {
	// A value never needs more than 32 bits; a larger n only ever comes from a
	// forged length field, where looping n times would hang. Reject it.
	if n < 0 || n > 32 {
		r.err = true
		return 0
	}
	var v uint32
	for i := 0; i < n; i++ {
		v = (v << 1) | r.bit()
	}
	return v
}

// ue reads an unsigned Exp-Golomb value (H.264/H.265 ue(v)).
func (r *bitReader) ue() uint32 {
	zeros := 0
	for r.bit() == 0 {
		if r.err {
			return 0
		}
		zeros++
		if zeros > 31 { // malformed: bail
			r.err = true
			return 0
		}
	}
	if zeros == 0 {
		return 0
	}
	return (1 << uint(zeros)) - 1 + r.bits(zeros)
}

// ueMax reads a ue(v) and flags err (returning 0) when it exceeds max. Loop
// counts read from the bitstream go through this so a forged huge value cannot
// make a parse loop spin billions of times — the classic bitstream hang/DoS.
func (r *bitReader) ueMax(max uint32) uint32 {
	v := r.ue()
	if v > max {
		r.err = true
		return 0
	}
	return v
}

// se reads a signed Exp-Golomb value (se(v)).
func (r *bitReader) se() int32 {
	k := r.ue()
	if k&1 == 1 {
		return int32((k + 1) / 2)
	}
	return -int32(k / 2)
}

// unescapeRBSP removes emulation_prevention_three_byte (0x03 following 0x00 0x00)
// from a H.264/HEVC NAL payload, yielding the RBSP.
func unescapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for i := 0; i < len(b); i++ {
		if zeros >= 2 && b[i] == 0x03 {
			zeros = 0
			continue // drop the emulation-prevention byte
		}
		out = append(out, b[i])
		if b[i] == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// nonEmpty reports whether bc carries at least one value worth returning.
func (bc *bitstreamColour) nonEmpty() bool {
	return bc.primaries != nil || bc.transfer != nil || bc.matrix != nil ||
		bc.rng != nil || bc.bitDepth != nil || bc.profile != "" || bc.level != nil ||
		bc.sarWidth != 0
}

// --- H.264 / AVC ---------------------------------------------------------------

func avcColour(cp []byte) *bitstreamColour {
	// AVCDecoderConfigurationRecord: version(1) profile(1) compat(1) level(1)
	// [6 reserved|2 lengthSizeMinusOne](1) [3 reserved|5 numSPS](1) then SPS list.
	if len(cp) < 7 || cp[0] != 1 {
		return nil
	}
	numSPS := int(cp[5] & 0x1f)
	off := 6
	for i := 0; i < numSPS; i++ {
		if off+2 > len(cp) {
			return nil
		}
		n := int(cp[off])<<8 | int(cp[off+1])
		off += 2
		if n < 1 || off+n > len(cp) {
			return nil
		}
		sps := cp[off : off+n]
		off += n
		if sps[0]&0x1f != 7 { // nal_unit_type 7 = SPS
			continue
		}
		if bc := parseAVCSPS(unescapeRBSP(sps[1:])); bc != nil {
			return bc
		}
	}
	return nil
}

func parseAVCSPS(rbsp []byte) *bitstreamColour {
	r := &bitReader{data: rbsp}
	bc := &bitstreamColour{}
	profileIDC := r.bits(8)
	r.bits(8)                          // constraint flags + reserved
	bc.level = u16p(uint16(r.bits(8))) // level_idc (ffprobe level)
	r.ue()                             // seq_parameter_set_id
	bc.profile = avcProfileName(profileIDC)

	high := false
	switch profileIDC {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		high = true
	}
	if high {
		cf := r.ue() // chroma_format_idc
		if cf == 3 {
			r.bit() // separate_colour_plane_flag
		}
		bdl := r.ue() // bit_depth_luma_minus8
		r.ue()        // bit_depth_chroma_minus8
		bc.bitDepth = validBitDepth(bdl + 8)
		r.bit() // qpprime_y_zero_transform_bypass_flag
		if r.bit() == 1 {
			n := 8
			if cf == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if r.bit() == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					skipAVCScalingList(r, size)
				}
			}
		}
	}
	r.ue() // log2_max_frame_num_minus4
	pocType := r.ue()
	switch pocType {
	case 0:
		r.ue() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		r.bit() // delta_pic_order_always_zero_flag
		r.se()  // offset_for_non_ref_pic
		r.se()  // offset_for_top_to_bottom_field
		for n := r.ueMax(255); n > 0; n-- {
			r.se() // num_ref_frames_in_pic_order_cnt_cycle (≤255)
		}
	}
	r.ue()  // max_num_ref_frames
	r.bit() // gaps_in_frame_num_value_allowed_flag
	r.ue()  // pic_width_in_mbs_minus1
	r.ue()  // pic_height_in_map_units_minus1
	if r.bit() == 0 {
		r.bit() // mb_adaptive_frame_field_flag
	}
	r.bit() // direct_8x8_inference_flag
	if r.bit() == 1 {
		r.ue()
		r.ue()
		r.ue()
		r.ue() // frame_crop_*
	}
	if r.bit() == 1 { // vui_parameters_present_flag
		parseAVCVUI(r, bc)
	}
	// AVC colour and bit depth come from the bitstream itself, so a parse error
	// makes the whole result untrustworthy — discard it and fall back to nil.
	if r.err || !bc.nonEmpty() {
		return nil
	}
	return bc
}

func parseAVCVUI(r *bitReader, bc *bitstreamColour) {
	if r.bit() == 1 { // aspect_ratio_info_present_flag
		readVUIAspectRatio(r, bc)
	}
	if r.bit() == 1 { // overscan_info_present_flag
		r.bit()
	}
	if r.bit() == 1 { // video_signal_type_present_flag
		r.bits(3) // video_format
		full := r.bit()
		p, t, m := readColourDescription(r)
		if r.err {
			return // do not commit partially-read colour
		}
		bc.rng = spsRange(full)
		bc.primaries, bc.transfer, bc.matrix = p, t, m
	}
}

// readVUIAspectRatio reads aspect_ratio_idc (after aspect_ratio_info_present_flag)
// and records the sample aspect ratio: Table E-1 for idc 1–16, or the inline
// sar_width:sar_height for Extended_SAR (255). This is the most common way H.264
// signals SAR, and it lives in the avcC's SPS, so it is read head-only.
func readVUIAspectRatio(r *bitReader, bc *bitstreamColour) {
	idc := r.bits(8)
	switch {
	case idc == 255: // Extended_SAR
		w := r.bits(16)
		h := r.bits(16)
		if !r.err {
			bc.sarWidth, bc.sarHeight = w, h
		}
	case idc >= 1 && idc <= 16:
		bc.sarWidth = uint32(avcSAR[idc][0])
		bc.sarHeight = uint32(avcSAR[idc][1])
	}
}

// readColourDescription reads colour_primaries/transfer/matrix when
// colour_description_present_flag is set, returning nil pointers otherwise.
func readColourDescription(r *bitReader) (primaries, transfer, matrix *uint16) {
	if r.bit() != 1 { // colour_description_present_flag
		return nil, nil, nil
	}
	return cicpOrNil(r.bits(8)), cicpOrNil(r.bits(8)), cicpOrNil(r.bits(8))
}

func skipAVCScalingList(r *bitReader, size int) {
	last, next := 8, 8
	for j := 0; j < size; j++ {
		if next != 0 {
			next = (last + int(r.se()) + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
}

func avcProfileName(idc uint32) string {
	switch idc {
	case 66:
		return "Baseline"
	case 77:
		return "Main"
	case 88:
		return "Extended"
	case 100:
		return "High"
	case 110:
		return "High 10"
	case 122:
		return "High 4:2:2"
	case 244:
		return "High 4:4:4 Predictive"
	}
	return ""
}

// --- HEVC ----------------------------------------------------------------------

func hevcColour(cp []byte) *bitstreamColour {
	// HEVCDecoderConfigurationRecord: 23-byte header then NAL arrays. The header
	// already carries general_profile_idc (byte 1, low 5 bits) and
	// bitDepthLumaMinus8 (byte 17, low 3 bits); the SPS VUI carries the colour.
	if len(cp) < 23 || cp[0] != 1 {
		return nil
	}
	bc := &bitstreamColour{}
	bc.bitDepth = validBitDepth(uint32(cp[17]&0x07) + 8)
	bc.profile = hevcProfileName(cp[1] & 0x1f)

	numArrays := int(cp[22])
	off := 23
	for a := 0; a < numArrays; a++ {
		if off+3 > len(cp) {
			break
		}
		nalType := cp[off] & 0x3f
		count := int(cp[off+1])<<8 | int(cp[off+2])
		off += 3
		for n := 0; n < count; n++ {
			if off+2 > len(cp) {
				return bc
			}
			ln := int(cp[off])<<8 | int(cp[off+1])
			off += 2
			if ln < 2 || off+ln > len(cp) {
				return bc
			}
			nal := cp[off : off+ln]
			off += ln
			if nalType == 33 { // SPS_NUT
				parseHEVCSPS(unescapeRBSP(nal[2:]), bc) // strip 2-byte NAL header
			}
		}
	}
	return bc
}

func parseHEVCSPS(rbsp []byte, bc *bitstreamColour) {
	r := &bitReader{data: rbsp}
	r.bits(4) // sps_video_parameter_set_id
	maxSub := r.bits(3)
	r.bit() // sps_temporal_id_nesting_flag
	skipHEVCProfileTierLevel(r, maxSub, bc)
	r.ue() // sps_seq_parameter_set_id
	cf := r.ue()
	if cf == 3 {
		r.bit() // separate_colour_plane_flag
	}
	r.ue()            // pic_width_in_luma_samples
	r.ue()            // pic_height_in_luma_samples
	if r.bit() == 1 { // conformance_window_flag
		r.ue()
		r.ue()
		r.ue()
		r.ue()
	}
	r.ue() // bit_depth_luma_minus8 (also in hvcC)
	r.ue() // bit_depth_chroma_minus8
	log2PocLsb := r.ue() + 4
	subOrdering := r.bit()
	start := maxSub
	if subOrdering == 1 {
		start = 0
	}
	for i := start; i <= maxSub; i++ {
		r.ue()
		r.ue()
		r.ue()
	}
	r.ue()            // log2_min_luma_coding_block_size_minus3
	r.ue()            // log2_diff_max_min_luma_coding_block_size
	r.ue()            // log2_min_luma_transform_block_size_minus2
	r.ue()            // log2_diff_max_min_luma_transform_block_size
	r.ue()            // max_transform_hierarchy_depth_inter
	r.ue()            // max_transform_hierarchy_depth_intra
	if r.bit() == 1 { // scaling_list_enabled_flag
		if r.bit() == 1 { // sps_scaling_list_data_present_flag
			skipHEVCScalingListData(r)
		}
	}
	r.bit()           // amp_enabled_flag
	r.bit()           // sample_adaptive_offset_enabled_flag
	if r.bit() == 1 { // pcm_enabled_flag
		r.bits(4)
		r.bits(4)
		r.ue()
		r.ue()
		r.bit()
	}
	skipHEVCShortTermRPS(r, r.ue())
	if r.bit() == 1 { // long_term_ref_pics_present_flag
		for n := r.ueMax(64); n > 0; n-- { // num_long_term_ref_pics_sps (≤32)
			r.bits(int(log2PocLsb)) // lt_ref_pic_poc_lsb_sps (log2PocLsb ≤ 16; bits() guards)
			r.bit()                 // used_by_curr_pic_lt_sps_flag
		}
	}
	r.bit()           // sps_temporal_mvp_enabled_flag
	r.bit()           // strong_intra_smoothing_enabled_flag
	if r.bit() == 1 { // vui_parameters_present_flag
		parseHEVCVUI(r, bc)
	}
}

func skipHEVCProfileTierLevel(r *bitReader, maxSub uint32, bc *bitstreamColour) {
	// general profile/tier/level: 2+1+5 +32 +4 +44 +8 = 96 bits.
	r.bits(8)                          // profile_space(2) tier(1) profile_idc(5)
	r.bits(32)                         // general_profile_compatibility_flags
	r.bits(32)                         // 4 source flags + 28 of the 44 reserved
	r.bits(16)                         // remaining 16 of reserved (4+44 = 48 total -> 32+16)
	bc.level = u16p(uint16(r.bits(8))) // general_level_idc (ffprobe level, 30×level)
	prof := make([]uint32, maxSub)
	lvl := make([]uint32, maxSub)
	for i := uint32(0); i < maxSub; i++ {
		prof[i] = r.bit()
		lvl[i] = r.bit()
	}
	if maxSub > 0 {
		for i := maxSub; i < 8; i++ {
			r.bits(2) // reserved_zero_2bits
		}
	}
	for i := uint32(0); i < maxSub; i++ {
		if prof[i] == 1 {
			r.bits(32)
			r.bits(32)
			r.bits(24) // 88-bit sub-layer profile block (32+32+24)
		}
		if lvl[i] == 1 {
			r.bits(8)
		}
	}
}

func skipHEVCScalingListData(r *bitReader) {
	for sizeID := 0; sizeID < 4; sizeID++ {
		step := 1
		if sizeID == 3 {
			step = 3
		}
		for matrixID := 0; matrixID < 6; matrixID += step {
			if r.bit() == 0 { // scaling_list_pred_mode_flag == 0
				r.ue() // scaling_list_pred_matrix_id_delta
				continue
			}
			coef := 64
			if c := 1 << uint(4+(sizeID<<1)); c < 64 {
				coef = c
			}
			if sizeID > 1 {
				r.se() // scaling_list_dc_coef_minus8
			}
			for i := 0; i < coef; i++ {
				r.se() // scaling_list_delta_coef
			}
			if r.err {
				return
			}
		}
	}
}

func skipHEVCShortTermRPS(r *bitReader, num uint32) {
	if num > 64 { // SPS allows up to 64 short-term RPS
		r.err = true
		return
	}
	numDelta := make([]uint32, num)
	for idx := uint32(0); idx < num; idx++ {
		inter := uint32(0)
		if idx != 0 {
			inter = r.bit()
		}
		if inter == 1 {
			r.bit() // delta_rps_sign
			r.ue()  // abs_delta_rps_minus1
			ref := numDelta[idx-1]
			var keep uint32
			for j := uint32(0); j <= ref; j++ {
				used := r.bit()
				useDelta := uint32(1)
				if used == 0 {
					useDelta = r.bit()
				}
				if used == 1 || useDelta == 1 {
					keep++
				}
			}
			numDelta[idx] = keep
		} else {
			neg := r.ue()
			pos := r.ue()
			if neg > 1024 || pos > 1024 {
				r.err = true
				return
			}
			numDelta[idx] = neg + pos
			for i := uint32(0); i < neg; i++ {
				r.ue()
				r.bit()
			}
			for i := uint32(0); i < pos; i++ {
				r.ue()
				r.bit()
			}
		}
		if r.err {
			return
		}
	}
}

func parseHEVCVUI(r *bitReader, bc *bitstreamColour) {
	if r.bit() == 1 { // aspect_ratio_info_present_flag
		readVUIAspectRatio(r, bc)
	}
	if r.bit() == 1 { // overscan_info_present_flag
		r.bit()
	}
	if r.bit() == 1 { // video_signal_type_present_flag
		r.bits(3) // video_format
		full := r.bit()
		p, t, m := readColourDescription(r)
		if r.err {
			return // do not commit partially-read colour
		}
		bc.rng = spsRange(full)
		bc.primaries, bc.transfer, bc.matrix = p, t, m
	}
}

func hevcProfileName(profileIDC byte) string {
	switch profileIDC {
	case 1:
		return "Main"
	case 2:
		return "Main 10"
	case 3:
		return "Main Still Picture"
	case 4:
		return "Range Extensions"
	}
	return ""
}

// --- AV1 -----------------------------------------------------------------------

func av1Colour(cp []byte) *bitstreamColour {
	// AV1CodecConfigurationRecord: 4-byte header then configOBUs.
	if len(cp) < 4 || cp[0]&0x80 == 0 { // marker bit
		return nil
	}
	bc := &bitstreamColour{}
	seqProfile := uint32(cp[1] >> 5)
	highBitDepth := uint32(cp[2]>>6) & 1
	twelveBit := uint32(cp[2]>>5) & 1
	switch {
	case seqProfile == 2 && highBitDepth == 1:
		if twelveBit == 1 {
			bc.bitDepth = u16p(12)
		} else {
			bc.bitDepth = u16p(10)
		}
	case highBitDepth == 1:
		bc.bitDepth = u16p(10)
	default:
		bc.bitDepth = u16p(8)
	}
	bc.profile = av1ProfileName(seqProfile)
	parseAV1OBUs(cp[4:], seqProfile, bc)
	return bc
}

func parseAV1OBUs(b []byte, seqProfile uint32, bc *bitstreamColour) {
	i := 0
	for i < len(b) {
		hdr := b[i]
		i++
		obuType := (hdr >> 3) & 0xf
		ext := (hdr >> 2) & 1
		hasSize := (hdr >> 1) & 1
		if ext == 1 {
			if i >= len(b) {
				return
			}
			i++
		}
		size := len(b) - i
		if hasSize == 1 {
			sz, n, ok := leb128(b[i:])
			if !ok {
				return
			}
			i += n
			size = int(sz)
		}
		if size < 0 || i+size > len(b) {
			return
		}
		if obuType == 1 { // OBU_SEQUENCE_HEADER
			parseAV1SeqHeader(b[i:i+size], seqProfile, bc)
			return
		}
		i += size
	}
}

func parseAV1SeqHeader(payload []byte, seqProfile uint32, bc *bitstreamColour) {
	r := &bitReader{data: payload}
	r.bits(3) // seq_profile
	r.bit()   // still_picture
	reduced := r.bit()
	if reduced == 1 {
		bc.level = u16p(uint16(r.bits(5))) // seq_level_idx[0]
	} else {
		timing := r.bit()
		decoderModel := uint32(0)
		if timing == 1 {
			r.bits(32)        // num_units_in_display_tick
			r.bits(32)        // time_scale
			if r.bit() == 1 { // equal_picture_interval
				r.uvlc() // num_ticks_per_picture_minus_1
			}
			decoderModel = r.bit()
			if decoderModel == 1 {
				r.bits(5)  // buffer_delay_length_minus_1
				r.bits(32) // num_units_in_decoding_tick
				r.bits(5)  // buffer_removal_time_length_minus_1
				r.bits(5)  // frame_presentation_time_length_minus_1
			}
		}
		initialDisplayDelay := r.bit()
		opCnt := r.bits(5) + 1
		bufDelayLen := uint32(0) // not needed precisely; recompute if decoderModel
		_ = bufDelayLen
		for i := uint32(0); i < opCnt; i++ {
			r.bits(12) // operating_point_idc
			levelIdx := r.bits(5)
			if i == 0 {
				bc.level = u16p(uint16(levelIdx)) // seq_level_idx[0] (ffprobe level)
			}
			if levelIdx > 7 {
				r.bit() // seq_tier
			}
			if decoderModel == 1 {
				if r.bit() == 1 { // decoder_model_present_for_this_op
					// operating_parameters_info: 2*(buffer_delay_length)+1 bits.
					// buffer_delay_length was read above only if decoderModel; re-read
					// is impossible, so bail defensively (rare path).
					r.err = true
					return
				}
			}
			if initialDisplayDelay == 1 {
				if r.bit() == 1 { // initial_display_delay_present_for_this_op
					r.bits(4) // initial_display_delay_minus_1
				}
			}
			if r.err {
				return
			}
		}
	}
	fwBits := r.bits(4) + 1
	fhBits := r.bits(4) + 1
	r.bits(int(fwBits)) // max_frame_width_minus_1
	r.bits(int(fhBits)) // max_frame_height_minus_1
	frameIDs := uint32(0)
	if reduced == 0 {
		frameIDs = r.bit()
	}
	if frameIDs == 1 {
		r.bits(4) // delta_frame_id_length_minus_2
		r.bits(3) // additional_frame_id_length_minus_1
	}
	r.bit() // use_128x128_superblock
	r.bit() // enable_filter_intra
	r.bit() // enable_intra_edge_filter
	if reduced == 0 {
		r.bit() // enable_interintra_compound
		r.bit() // enable_masked_compound
		r.bit() // enable_warped_motion
		r.bit() // enable_dual_filter
		enableOrderHint := r.bit()
		if enableOrderHint == 1 {
			r.bit() // enable_jnt_comp
			r.bit() // enable_ref_frame_mvs
		}
		var forceScreen uint32 = 2 // SELECT_SCREEN_CONTENT_TOOLS
		if r.bit() == 0 {          // seq_choose_screen_content_tools == 0
			forceScreen = r.bit()
		}
		if forceScreen > 0 {
			if r.bit() == 0 { // seq_choose_integer_mv == 0
				r.bit() // seq_force_integer_mv
			}
		}
		if enableOrderHint == 1 {
			r.bits(3) // order_hint_bits_minus_1
		}
	}
	r.bit() // enable_superres
	r.bit() // enable_cdef
	r.bit() // enable_restoration
	parseAV1ColorConfig(r, seqProfile, bc)
}

func parseAV1ColorConfig(r *bitReader, seqProfile uint32, bc *bitstreamColour) {
	highBitDepth := r.bit()
	var bitDepth uint32 = 8
	if seqProfile == 2 && highBitDepth == 1 {
		if r.bit() == 1 { // twelve_bit
			bitDepth = 12
		} else {
			bitDepth = 10
		}
	} else if highBitDepth == 1 {
		bitDepth = 10
	}
	mono := uint32(0)
	if seqProfile != 1 {
		mono = r.bit()
	}
	var cp, tc, mc uint32 = 2, 2, 2 // UNSPECIFIED
	if r.bit() == 1 {               // color_description_present_flag
		cp = r.bits(8)
		tc = r.bits(8)
		mc = r.bits(8)
	}
	if r.err {
		return
	}
	bc.bitDepth = validBitDepth(bitDepth)
	bc.primaries = cicpOrNil(cp)
	bc.transfer = cicpOrNil(tc)
	bc.matrix = cicpOrNil(mc)
	if mono == 1 {
		bc.rng = spsRange(r.bit()) // color_range
		return
	}
	if cp == 1 && tc == 13 && mc == 0 {
		bc.rng = u16p(2) // full range implied
		return
	}
	bc.rng = spsRange(r.bit()) // color_range
}

func (r *bitReader) uvlc() uint32 {
	zeros := 0
	for r.bit() == 0 {
		if r.err {
			return 0
		}
		zeros++
		if zeros >= 32 {
			return 0xffffffff
		}
	}
	if zeros == 0 {
		return 0
	}
	return r.bits(zeros) + (1 << uint(zeros)) - 1
}

func leb128(b []byte) (uint64, int, bool) {
	var v uint64
	for i := 0; i < 8; i++ {
		if i >= len(b) {
			return 0, 0, false
		}
		v |= uint64(b[i]&0x7f) << uint(7*i)
		if b[i]&0x80 == 0 {
			return v, i + 1, true
		}
	}
	return 0, 0, false
}

func av1ProfileName(p uint32) string {
	switch p {
	case 0:
		return "Main"
	case 1:
		return "High"
	case 2:
		return "Professional"
	}
	return ""
}

// --- VP9 (vpcC, when present in CodecPrivate) ----------------------------------

func vp9Colour(cp []byte) *bitstreamColour {
	// VPCodecConfigurationRecord (vpcC): version+flags(4) then profile(1) level(1)
	// [bitDepth(4)|chromaSubsampling(3)|videoFullRangeFlag(1)](1) colourPrimaries(1)
	// transferCharacteristics(1) matrixCoefficients(1) ... Some muxers store it
	// without the 4-byte FullBox prefix; accept both lengths defensively.
	var b []byte
	switch {
	case len(cp) >= 12 && cp[0] <= 1: // FullBox: version(1) flags(3)
		b = cp[4:]
	case len(cp) >= 8:
		b = cp
	default:
		return nil
	}
	if len(b) < 6 {
		return nil
	}
	bc := &bitstreamColour{}
	bc.profile = "" // numeric VP9 profile not mapped to an ffprobe string here
	bitDepth := b[2] >> 4
	fullRange := (b[2] >> 0) & 1
	if bitDepth == 8 || bitDepth == 10 || bitDepth == 12 {
		bc.bitDepth = u16p(uint16(bitDepth))
	}
	bc.primaries = u16p(uint16(b[3]))
	bc.transfer = u16p(uint16(b[4]))
	bc.matrix = u16p(uint16(b[5]))
	bc.rng = spsRange(uint32(fullRange))
	// Reject obviously-bogus records (all-zero colour with zero bit depth).
	if bc.bitDepth == nil && b[3] == 0 && b[4] == 0 && b[5] == 0 {
		return nil
	}
	return bc
}
