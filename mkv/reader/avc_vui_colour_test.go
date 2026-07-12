package reader

import (
	"encoding/hex"
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

// spsPrefix builds a High-profile SPS up to (and including) the
// vui_parameters_present_flag, leaving the caller to append the VUI body.
func spsPrefix() *tbw {
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
	return b
}

// wrapAvcC wraps a completed SPS RBSP in an AVCDecoderConfigurationRecord.
func wrapAvcC(b *tbw) []byte {
	nal := append([]byte{0x67}, b.bytes()...) // NAL header: SPS (type 7)
	avcc := []byte{0x01, 100, 0, 40, 0xFF, 0xE1}
	avcc = append(avcc, byte(len(nal)>>8), byte(len(nal)))
	return append(avcc, nal...)
}

// buildHighSPSAvcC builds an avcC for a High-profile SPS whose VUI carries the
// given colour code points (no aspect_ratio_info, no container colr).
func buildHighSPSAvcC(primaries, transfer, matrix uint32) []byte {
	b := spsPrefix()
	b.u(0, 1)         // aspect_ratio_info_present_flag
	b.u(0, 1)         // overscan_info_present_flag
	b.u(1, 1)         // video_signal_type_present_flag
	b.u(5, 3)         // video_format
	b.u(0, 1)         // video_full_range_flag
	b.u(1, 1)         // colour_description_present_flag
	b.u(primaries, 8) // colour_primaries
	b.u(transfer, 8)  // transfer_characteristics
	b.u(matrix, 8)    // matrix_coefficients
	return wrapAvcC(b)
}

// buildSPSAspectAvcC builds an avcC for a High-profile SPS whose VUI carries
// aspect_ratio_info (idc, or Extended_SAR 255 with sarW:sarH) and no colour.
func buildSPSAspectAvcC(idc, sarW, sarH uint32) []byte {
	b := spsPrefix()
	b.u(1, 1)   // aspect_ratio_info_present_flag
	b.u(idc, 8) // aspect_ratio_idc
	if idc == 255 {
		b.u(sarW, 16)
		b.u(sarH, 16)
	}
	b.u(0, 1) // overscan_info_present_flag
	b.u(0, 1) // video_signal_type_present_flag = 0 (no colour)
	return wrapAvcC(b)
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

// TestAVCVUIAspectRatio covers the SAR read from the SPS VUI aspect_ratio_info  -
// the most common H.264 SAR signalling, head-only in the avcC. No pasp box.
func TestAVCVUIAspectRatio(t *testing.T) {
	w, h := uint32(1280), uint32(720)
	check := func(avcc []byte, wantSAR, wantDAR string) {
		t.Helper()
		tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc, Width: &w, Height: &h}
		fillColourFromCodecPrivate(&tr)
		if tr.SampleAspectRatio() != wantSAR || tr.DisplayAspectRatio() != wantDAR {
			t.Errorf("sar=%q dar=%q, want %s / %s", tr.SampleAspectRatio(), tr.DisplayAspectRatio(), wantSAR, wantDAR)
		}
	}
	// Extended_SAR 257:160 (Goal 2.mp4) over 1280x720 → DAR = SAR*W/H = 257:90.
	check(buildSPSAspectAvcC(255, 257, 160), "257:160", "257:90")
	// Predefined idc 14 = 4:3 → DAR = (1280*4):(720*3) = 64:27.
	check(buildSPSAspectAvcC(14, 0, 0), "4:3", "64:27")
	// idc 1 = square (1:1) → no display dims set, DAR is the coded 16:9.
	check(buildSPSAspectAvcC(1, 0, 0), "1:1", "16:9")
}

// TestPaspTakesPrecedenceOverVUI: when both a container/pasp display aspect and a
// VUI SAR exist, the former wins (the VUI only fills a gap).
func TestPaspTakesPrecedenceOverVUI(t *testing.T) {
	w, h := uint32(1280), uint32(720)
	dw, dh := uint32(4), uint32(3) // pre-set display aspect 4:3 (e.g. from pasp)
	tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: buildSPSAspectAvcC(255, 257, 160),
		Width: &w, Height: &h, DisplayWidth: &dw, DisplayHeight: &dh}
	fillColourFromCodecPrivate(&tr)
	if tr.DisplayWidth == nil || *tr.DisplayWidth != 4 || *tr.DisplayHeight != 3 {
		t.Errorf("pasp display aspect must win, got %v:%v", tr.DisplayWidth, tr.DisplayHeight)
	}
}

// TestAVCColourInBandSPSOnly documents an accepted head-only limitation. This is
// a real Main@L4.2 avcC (1280x720) whose SPS sets video_signal_type_present_flag
// = 0 - it carries NO colour. The file's BT.709 lives only in a second, in-band
// SPS an external prober reaches by decoding a frame; head-only mkvgo reads the avcC's
// SPS and correctly reports no colour. Same class as implicit in-band SBR/PS: the
// data is in no header we parse, so reporting "" is correct, not a bug.
func TestAVCColourInBandSPSOnly(t *testing.T) {
	avcc, err := hex.DecodeString("014d042affe1000e674d042ae900a00b7403c2211a8001000468ee3c80")
	if err != nil {
		t.Fatal(err)
	}
	tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc}
	fillColourFromCodecPrivate(&tr)
	// The avcC SPS is parsed correctly (profile/level), but it has no VUI colour.
	if tr.Profile != "Main" || tr.Level == nil || *tr.Level != 42 {
		t.Errorf("avcC parse: profile=%q level=%v, want Main / 42", tr.Profile, p16(tr.Level))
	}
	if tr.ColorSpace != nil {
		t.Errorf("color_space should be nil (this avcC SPS carries no colour), got %d", *tr.ColorSpace)
	}
}
