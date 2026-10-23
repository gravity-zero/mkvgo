package mp4

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func (w *bitWriter) ue(v uint32) {
	n := 0
	for (v+1)>>n > 1 {
		n++
	}
	w.write(0, n)
	w.write(v+1, n+1)
}

func (w *bitWriter) se(v int32) {
	if v > 0 {
		w.ue(uint32(2*v - 1))
	} else {
		w.ue(uint32(-2 * v))
	}
}

// trailing ends an RBSP: a stop bit, then zeros to the byte.
func (w *bitWriter) trailing() []byte {
	w.write(1, 1)
	return w.bytes()
}

// escapeRBSP inserts the emulation prevention bytes a NAL payload needs.
func escapeRBSP(rbsp []byte) []byte {
	var out []byte
	zeros := 0
	for _, c := range rbsp {
		if zeros >= 2 && c <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// testAVCSPS is a Baseline SPS (id 0) with a 16-bit frame_num and POC LSB, which
// a slice header writes as runs of zero bits.
func testAVCSPS() []byte {
	var w bitWriter
	w.write(66, 8)
	w.write(0, 8)
	w.write(30, 8)
	w.ue(0)  // seq_parameter_set_id
	w.ue(12) // log2_max_frame_num_minus4
	w.ue(0)  // pic_order_cnt_type
	w.ue(12) // log2_max_pic_order_cnt_lsb_minus4
	w.ue(1)  // max_num_ref_frames
	w.write(0, 1)
	w.ue(3) // pic_width_in_mbs_minus1
	w.ue(2) // pic_height_in_map_units_minus1
	w.write(1, 1)
	w.write(1, 1) // direct_8x8_inference_flag
	w.write(0, 2) // frame_cropping_flag, vui_parameters_present_flag
	return append([]byte{0x67}, escapeRBSP(w.trailing())...)
}

func testAVCPPS() []byte { return testAVCPPSWith(false) }

// testAVCPPSWith is PPS 0, redundant_pic_cnt_present_flag set when redundant.
func testAVCPPSWith(redundant bool) []byte {
	var w bitWriter
	w.ue(0)       // pic_parameter_set_id
	w.ue(0)       // seq_parameter_set_id
	w.write(0, 2) // entropy_coding_mode_flag, bottom_field_pic_order_in_frame_present_flag
	w.ue(0)       // num_slice_groups_minus1
	w.ue(0)
	w.ue(0)
	w.write(0, 3) // weighted_pred_flag, weighted_bipred_idc
	w.se(0)
	w.se(0)
	w.se(0)
	w.write(1, 1) // deblocking_filter_control_present_flag
	w.write(0, 1)
	if redundant {
		w.write(1, 1)
	} else {
		w.write(0, 1)
	}
	return append([]byte{0x68}, escapeRBSP(w.trailing())...)
}

// testAVCSlice is an IDR I slice of payload slice data bytes, and the size of
// its NAL and slice headers once escaped. An idr_pic_id of 65535 follows
// frame_num's zero bits with 16 more, escaped.
func testAVCSlice(firstMB, idrPicID uint32, payload int) (nal []byte, header int) {
	return testAVCSliceWith(firstMB, idrPicID, payload, false)
}

// testAVCSliceWith writes a redundant_pic_cnt of 255 when redundant (testAVCPPSWith).
func testAVCSliceWith(firstMB, idrPicID uint32, payload int, redundant bool) (nal []byte, header int) {
	var w bitWriter
	w.ue(firstMB)
	w.ue(7)        // slice_type: I, all slices
	w.ue(0)        // pic_parameter_set_id
	w.write(0, 16) // frame_num
	w.ue(idrPicID)
	w.write(0, 16) // pic_order_cnt_lsb
	if redundant {
		w.ue(255)
	}
	w.write(0, 2) // dec_ref_pic_marking
	w.se(0)       // slice_qp_delta
	w.ue(1)       // disable_deblocking_filter_idc
	bitsUsed := len(w.b)*8 + int(w.nbits)
	for range payload {
		w.write(0xA5, 8)
	}
	rbsp := w.trailing()
	escaped := escapeRBSP(rbsp)
	header = len(escapeRBSP(rbsp[:(bitsUsed+7)/8]))
	return append([]byte{0x65}, escaped...), 1 + header
}

// testAVCFrame is a size-byte H.264 sample: in-band SPS and PPS, then an IDR slice.
func testAVCFrame(size int) []byte {
	head := lengthPrefixed(4, testAVCSPS(), testAVCPPS())
	slice, _ := testAVCSlice(0, 0, 0)
	slice, _ = testAVCSlice(0, 0, size-len(head)-4-len(slice))
	return append(head, lengthPrefixed(4, slice)...)
}

func lengthPrefixed(lenSize int, nals ...[]byte) []byte {
	var out []byte
	for _, n := range nals {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(n)))
		out = append(out, l[4-lenSize:]...)
		out = append(out, n...)
	}
	return out
}

func TestH264SliceHeaderLeftClear(t *testing.T) {
	sps, pps := testAVCSPS(), testAVCPPS()
	sei := []byte{0x06, 0x05, 0x02, 0xAA, 0xBB, 0x80}
	slice, hdr := testAVCSlice(0, 65535, 300)
	if !bytes.Contains(slice[:hdr], []byte{0, 0, 3}) {
		t.Fatal("the fixture's slice header holds no emulation prevention byte")
	}
	sample := lengthPrefixed(4, sps, pps, sei, slice)
	lead := 4 + len(sps) + 4 + len(pps) + 4 + len(sei) + 4 + hdr
	for _, c := range []struct {
		scheme string
		want   []cencSubsample
	}{
		{"cbcs", []cencSubsample{{clear: uint16(lead), protected: uint32(len(slice) - hdr)}}},
		{"cenc", []cencSubsample{{clear: uint16(lead + (len(slice)-hdr)%16), protected: uint32((len(slice) - hdr) / 16 * 16)}}},
	} {
		s, err := newH26xSplitter("h264", nil, c.scheme)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.split(sample)
		if err != nil {
			t.Fatalf("%s: %v", c.scheme, err)
		}
		if len(got) != 1 || got[0] != c.want[0] {
			t.Errorf("%s: subsamples %+v, want %+v", c.scheme, got, c.want)
		}
	}
}

func TestH264SlicesOfOnePicture(t *testing.T) {
	avcC := append([]byte{1, 66, 0, 30, 0xFD, 0xE1}, lengthPrefixed(2, testAVCSPS())...)
	avcC = append(append(avcC, 1), lengthPrefixed(2, testAVCPPS())...)
	var nals [][]byte
	var want []cencSubsample
	for i := range 3 {
		nal, hdr := testAVCSlice(uint32(i*4), 0, 100+i*7)
		nals = append(nals, nal)
		want = append(want, cencSubsample{clear: uint16(2 + hdr), protected: uint32(len(nal) - hdr)})
	}
	s, err := newH26xSplitter("h264", avcC, "cbcs")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.split(lengthPrefixed(2, nals...))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d subsamples, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("slice %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestH26xSplitRefusals(t *testing.T) {
	slice, _ := testAVCSlice(0, 0, 40)
	s, _ := newH26xSplitter("h264", nil, "cenc")
	if _, err := s.split(lengthPrefixed(4, slice)); err == nil {
		t.Error("a slice whose parameter sets the stream never carried was split")
	}
	if _, err := newH26xSplitter("h264", []byte{1, 66, 0, 30, 0xFE, 0xE0, 0}, "cenc"); err == nil {
		t.Error("3-byte NAL lengths accepted")
	}
	s, _ = newH26xSplitter("h264", nil, "cenc")
	if _, err := s.split(append(lengthPrefixed(4, testAVCSPS()), 0, 0, 0)); err == nil {
		t.Error("a truncated length prefix accepted")
	}
}

func TestH26xLongClearRunSplits(t *testing.T) {
	sei := bytes.Repeat([]byte{0x06}, 70000)
	slice, hdr := testAVCSlice(0, 0, 64)
	s, _ := newH26xSplitter("h264", nil, "cbcs")
	got, err := s.split(lengthPrefixed(4, testAVCSPS(), testAVCPPS(), sei, slice))
	if err != nil {
		t.Fatal(err)
	}
	clear := 4 + len(testAVCSPS()) + 4 + len(testAVCPPS()) + 4 + len(sei) + 4 + hdr
	want := []cencSubsample{{clear: 0xFFFF}, {clear: uint16(clear - 0xFFFF), protected: uint32(len(slice) - hdr)}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("%+v, want %+v", got, want)
	}
}

func TestNALReaderSkipsEmulationPrevention(t *testing.T) {
	r := newNALReader([]byte{0, 0, 3, 1, 0xFF})
	if v := r.u(24); v != 1 {
		t.Fatalf("read %#x, want 0x000001", v)
	}
	if n, _ := r.headerLen(); n != 4 {
		t.Errorf("%d escaped bytes, want 4", n)
	}
}
