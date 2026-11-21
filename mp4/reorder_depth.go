package mp4

// reorder_depth.go - the frame reordering depth a video track's parameter sets declare.

import (
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
)

// maxReorderDepth is the deepest reordering H.264 and HEVC allow (their decoded picture buffer holds 16 frames).
const maxReorderDepth = 16

// declaredReorderDepth is how many frames the track's H.264/HEVC parameter sets allow to precede a frame in decode
// order and follow it in output order (max_num_reorder_frames, sps_max_num_reorder_pics); 0 when nothing states it.
func declaredReorderDepth(t *mkv.Track) int {
	hevc := t.Codec == "hevc"
	if !hevc && t.Codec != "h264" {
		return 0
	}
	sets, _, ok := configParameterSets(hevc, t.CodecPrivate)
	if !ok {
		return 0
	}
	depth := 0
	for _, nal := range sets {
		switch {
		case hevc && len(nal) > 2 && nal[0]>>1&0x3f == 33:
			if _, sps, ok := parseHEVCSPS(newNALReader(nal[2:])); ok {
				depth = max(depth, sps.numReorderPics)
			}
		case !hevc && len(nal) > 1 && nal[0]&0x1f == 7:
			depth = max(depth, avcReorderFrames(newNALReader(nal[1:])))
		}
	}
	return depth
}

// avcReorderFrames reads an H.264 SPS through its VUI to max_num_reorder_frames (ITU-T H.264 E.1.1); 0 when the SPS
// carries no bitstream restriction or cannot be read.
func avcReorderFrames(r *nalReader) int {
	_, sps, ok := parseAVCSPS(r)
	if !ok {
		return 0
	}
	if !sps.frameMbsOnly {
		r.skip(1) // mb_adaptive_frame_field_flag
	}
	r.skip(1)     // direct_8x8_inference_flag
	if r.flag() { // frame_cropping_flag
		r.ue()
		r.ue()
		r.ue()
		r.ue()
	}
	if !r.flag() { // vui_parameters_present_flag
		return 0
	}
	if r.flag() && r.u(8) == 255 { // aspect_ratio_info_present_flag, Extended_SAR
		r.skip(32)
	}
	if r.flag() { // overscan_info_present_flag
		r.skip(1)
	}
	if r.flag() { // video_signal_type_present_flag
		r.skip(4)
		if r.flag() { // colour_description_present_flag
			r.skip(24)
		}
	}
	if r.flag() { // chroma_loc_info_present_flag
		r.ue()
		r.ue()
	}
	if r.flag() { // timing_info_present_flag
		r.skip(65)
	}
	nalHRD := r.flag()
	if nalHRD {
		skipAVCHRD(r)
	}
	vclHRD := r.flag()
	if vclHRD {
		skipAVCHRD(r)
	}
	if nalHRD || vclHRD {
		r.skip(1) // low_delay_hrd_flag
	}
	r.skip(1)      // pic_struct_present_flag
	if !r.flag() { // bitstream_restriction_flag
		return 0
	}
	r.skip(1) // motion_vectors_over_pic_boundaries_flag
	r.ue()    // max_bytes_per_pic_denom
	r.ue()    // max_bits_per_mb_denom
	r.ue()    // log2_max_mv_length_horizontal
	r.ue()    // log2_max_mv_length_vertical
	n := r.ue()
	if r.err || n > maxReorderDepth {
		return 0
	}
	return int(n)
}

func skipAVCHRD(r *nalReader) {
	cnt := r.ueMax(31) // cpb_cnt_minus1
	r.skip(8)          // bit_rate_scale, cpb_size_scale
	for i := uint32(0); i <= cnt && !r.err; i++ {
		r.ue()
		r.ue()
		r.skip(1)
	}
	r.skip(20)
}

// prefixFrameTS is the longest step between two neighbouring frames of the prefix, once in presentation order. A step
// of two frames or more is a frame the prefix does not hold yet (it decodes later), not a frame duration.
func prefixFrameTS(ptsTS []int64) int64 {
	if len(ptsTS) < 2 {
		return 0
	}
	sorted := make([]int64, len(ptsTS))
	copy(sorted, ptsTS)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var shortest int64
	for i := 1; i < len(sorted); i++ {
		if d := sorted[i] - sorted[i-1]; d > 0 && (shortest == 0 || d < shortest) {
			shortest = d
		}
	}
	var longest int64
	for i := 1; i < len(sorted); i++ {
		if d := sorted[i] - sorted[i-1]; d > longest && d < 2*shortest {
			longest = d
		}
	}
	return longest
}
