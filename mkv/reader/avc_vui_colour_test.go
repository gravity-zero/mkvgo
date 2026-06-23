package reader

import (
	"math/bits"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// tbw is a minimal MSB-first bit writer for hand-building an SPS RBSP in tests.
type tbw struct{ bits []byte }

func (w *tbw) u(val uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		w.bits = append(w.bits, byte((val>>uint(i))&1))
	}
}

// ue writes an unsigned Exp-Golomb codeNum.
func (w *tbw) ue(val uint32) {
	v := val + 1
	n := bits.Len32(v) - 1
	for i := 0; i < n; i++ {
		w.bits = append(w.bits, 0)
	}
	w.u(v, n+1)
}

func (w *tbw) bytes() []byte {
	out := make([]byte, (len(w.bits)+7)/8)
	for i, b := range w.bits {
		if b != 0 {
			out[i/8] |= 1 << (7 - uint(i%8))
		}
	}
	return out
}

// buildHighSPSAvcC builds an avcC for a High-profile SPS whose VUI carries the
// given colour code points (no container colr).
func buildHighSPSAvcC(primaries, transfer, matrix uint32) []byte {
	b := &tbw{}
	b.u(100, 8) // profile_idc = High
	b.u(0, 8)   // constraint flags + reserved
	b.u(40, 8)  // level_idc
	b.ue(0)     // seq_parameter_set_id
	b.ue(1)     // chroma_format_idc = 1 (4:2:0)
	b.ue(0)     // bit_depth_luma_minus8
	b.ue(0)     // bit_depth_chroma_minus8
	b.u(0, 1)   // qpprime_y_zero_transform_bypass_flag
	b.u(0, 1)   // seq_scaling_matrix_present_flag = 0
	b.ue(0)     // log2_max_frame_num_minus4
	b.ue(0)     // pic_order_cnt_type = 0
	b.ue(0)     // log2_max_pic_order_cnt_lsb_minus4
	b.ue(1)     // max_num_ref_frames
	b.u(0, 1)   // gaps_in_frame_num_value_allowed_flag
	b.ue(29)    // pic_width_in_mbs_minus1
	b.ue(17)    // pic_height_in_map_units_minus1
	b.u(1, 1)   // frame_mbs_only_flag = 1
	b.u(1, 1)   // direct_8x8_inference_flag
	b.u(0, 1)   // frame_cropping_flag = 0
	b.u(1, 1)   // vui_parameters_present_flag = 1
	// VUI
	b.u(0, 1)         // aspect_ratio_info_present_flag
	b.u(0, 1)         // overscan_info_present_flag
	b.u(1, 1)         // video_signal_type_present_flag
	b.u(5, 3)         // video_format
	b.u(0, 1)         // video_full_range_flag
	b.u(1, 1)         // colour_description_present_flag
	b.u(primaries, 8) // colour_primaries
	b.u(transfer, 8)  // transfer_characteristics
	b.u(matrix, 8)    // matrix_coefficients

	rbsp := b.bytes()
	nal := append([]byte{0x67}, rbsp...) // NAL header: SPS (type 7)
	avcc := []byte{0x01, 100, 0, 40, 0xFF, 0xE1}
	avcc = append(avcc, byte(len(nal)>>8), byte(len(nal)))
	return append(avcc, nal...)
}

// TestAVCVUIMatrixOnly reproduces the reported case: SDR H.264 with NO colr box,
// whose SPS VUI specifies only the matrix (BT.709 = 1) while leaving
// primaries/transfer unspecified (2). The matrix must be read head-only.
func TestAVCVUIMatrixOnly(t *testing.T) {
	tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: buildHighSPSAvcC(2, 2, 1)}
	fillColourFromCodecPrivate(&tr)
	t.Logf("primaries=%v transfer=%v matrix=%v range=%v",
		p16(tr.ColorPrimaries), p16(tr.ColorTransfer), p16(tr.ColorSpace), p16(tr.ColorRange))
	if tr.ColorSpaceName() != "bt709" {
		t.Errorf("color_space = %q, want bt709 (matrix read independently of unspecified primaries/transfer)", tr.ColorSpaceName())
	}
	if tr.ColorPrimaries != nil {
		t.Errorf("primaries should stay nil (unspecified), got %d", *tr.ColorPrimaries)
	}
}
