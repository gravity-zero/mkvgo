package ops

import (
	"bytes"
	"testing"
)

// bitWriter builds RBSPs for synthetic parameter sets and slice headers.
type bitWriter struct {
	out  []byte
	nbit int
}

func (w *bitWriter) u(n int, v uint32) {
	for i := n - 1; i >= 0; i-- {
		if w.nbit%8 == 0 {
			w.out = append(w.out, 0)
		}
		if v>>uint(i)&1 == 1 {
			w.out[len(w.out)-1] |= 0x80 >> uint(w.nbit%8)
		}
		w.nbit++
	}
}

func (w *bitWriter) ue(v uint32) {
	v++
	n := 0
	for x := v; x > 1; x >>= 1 {
		n++
	}
	w.u(n, 0)
	w.u(n+1, v)
}

// rbsp closes the RBSP with its stop bit.
func (w *bitWriter) rbsp() []byte {
	w.u(1, 1)
	for w.nbit%8 != 0 {
		w.u(1, 0)
	}
	return w.out
}

func hevcNAL(typ byte, rbsp []byte) []byte {
	return appendEPB([]byte{typ << 1, 0x01}, rbsp)
}

// 64x48, 8-bit POC LSB, 16x16 CTBs, one PPS.
func testHEVCSPS() []byte {
	var w bitWriter
	w.u(4, 0)
	w.u(3, 0)
	w.u(1, 1)
	w.u(32, 0x01600000) // general profile space/tier/idc + compat flags
	w.u(32, 0)
	w.u(32, 0) // constraint flags + level
	w.ue(0)    // sps id
	w.ue(1)    // 4:2:0
	w.ue(64)
	w.ue(48)
	w.u(1, 0)
	w.ue(0)
	w.ue(0)
	w.ue(4) // log2_max_pic_order_cnt_lsb_minus4: 8-bit LSB
	w.u(1, 1)
	w.ue(4)
	w.ue(2)
	w.ue(0)
	w.ue(0) // log2_min_luma_coding_block_size_minus3
	w.ue(1) // log2_diff_max_min: 16x16 CTB
	w.u(8, 0xA5)
	return hevcNAL(33, w.rbsp())
}

func testHEVCPPS() []byte {
	var w bitWriter
	w.ue(0)
	w.ue(0)
	w.u(1, 0)
	w.u(1, 0)
	w.u(3, 0)
	w.u(8, 0x5A)
	return hevcNAL(34, w.rbsp())
}

// testHEVCSlice is a first slice segment of an IRAP picture of type typ with
// POC LSB lsb, followed by tail as slice data.
func testHEVCSlice(typ byte, lsb uint32, tail []byte) []byte {
	var w bitWriter
	w.u(1, 1) // first_slice_segment_in_pic_flag
	w.u(1, 0) // no_output_of_prior_pics_flag
	w.ue(0)   // pps id
	w.ue(2)   // I slice
	if typ != 19 && typ != 20 {
		w.u(8, lsb)
	}
	for _, b := range tail {
		w.u(8, uint32(b))
	}
	return hevcNAL(typ, w.rbsp())
}

func testHVCC() []byte {
	rec := make([]byte, 23)
	rec[0] = 1
	rec[21] = 0x03 // NAL length size 4
	rec[22] = 2
	for _, nal := range [][]byte{testHEVCSPS(), testHEVCPPS()} {
		rec = append(rec, 0x80|(nal[0]>>1), 0, 1, byte(len(nal)>>8), byte(len(nal)))
		rec = append(rec, nal...)
	}
	return rec
}

// sliceLSB reads back the POC LSB of a renumbered first slice segment.
func sliceLSB(t *testing.T, sample []byte) uint32 {
	t.Helper()
	var lsb uint32
	found := false
	_ = forEachNAL(sample, 4, func(nal []byte) bool {
		if typ := (nal[0] >> 1) & 0x3F; typ >= 16 && typ <= 21 {
			r := expGolomb{data: appendRBSP(nil, nal[2:])}
			r.bits(2)
			r.ue()
			r.ue()
			lsb, found = r.bits(8), true
			return false
		}
		return true
	})
	if !found {
		t.Fatalf("no IRAP slice in %x", sample)
	}
	return lsb
}

// Successive CRA keyframes count up by a quarter of the LSB range, wrapping;
// an IDR restarts the count; the bytes after the POC field are untouched,
// emulation prevention included.
func TestHEVCPOCRenumber(t *testing.T) {
	h := newHEVCPOCRenumber(testHVCC())
	// Slice data holding a pattern that needs emulation prevention.
	tail := []byte{0x00, 0x00, 0x01, 0x7F, 0x00, 0x00}
	steps := []struct {
		typ  byte
		want uint32
	}{{21, 64}, {21, 128}, {16, 192}, {21, 0}, {21, 64}, {19, 0}, {21, 64}}
	for i, st := range steps {
		in := lp(testHEVCSlice(st.typ, 0xD3, tail))
		out, err := h.renumber(nil, in, 4)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if st.typ == 19 {
			if !bytes.Equal(out, in) {
				t.Fatalf("IDR rewritten")
			}
			continue
		}
		if got := sliceLSB(t, out); got != st.want {
			t.Fatalf("step %d: LSB %d, want %d", i, got, st.want)
		}
		// Everything but the 8 LSB bits is the input's.
		if want := lp(testHEVCSlice(st.typ, st.want, tail)); !bytes.Equal(out, want) {
			t.Fatalf("step %d:\n got %x\nwant %x", i, out, want)
		}
	}
}

// In-band parameter sets are learned; a slice naming an unknown PPS is an
// error (the sample is then skipped, not emitted with a stale POC).
func TestHEVCPOCRenumberInBandAndUnknown(t *testing.T) {
	h := newHEVCPOCRenumber(nil)
	if _, err := h.renumber(nil, lp(testHEVCSlice(21, 1, nil)), 4); err == nil {
		t.Fatal("slice without parameter sets renumbered")
	}
	in := lp(testHEVCSPS(), testHEVCPPS(), testHEVCSlice(21, 1, []byte{9}))
	out, err := h.renumber(nil, in, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got := sliceLSB(t, out); got != 64 { // the failed one is not counted
		t.Fatalf("LSB %d", got)
	}
}

// Baseline, 4-bit frame_num, POC type 0 with a 6-bit LSB, frame MBs only.
func testAVCSPS() []byte {
	var w bitWriter
	w.u(8, 66)
	w.u(8, 0)
	w.u(8, 30)
	w.ue(0)
	w.ue(0) // log2_max_frame_num_minus4
	w.ue(0) // pic_order_cnt_type
	w.ue(2) // log2_max_pic_order_cnt_lsb_minus4
	w.ue(1)
	w.u(1, 0)
	w.ue(19)
	w.ue(14)
	w.u(1, 1) // frame_mbs_only_flag
	return appendEPB([]byte{0x67}, w.rbsp())
}

func testAVCPPS() []byte {
	var w bitWriter
	w.ue(0)
	w.ue(0)
	w.u(2, 0)
	return appendEPB([]byte{0x68}, w.rbsp())
}

// testAVCSlice is a slice NAL (header byte hdr) of slice_type st with the
// given frame_num and POC LSB, then a few slice data bytes.
func testAVCSlice(hdr byte, st, frameNum, lsb uint32) []byte {
	var w bitWriter
	w.ue(0) // first_mb_in_slice
	w.ue(st)
	w.ue(0) // pps id
	w.u(4, frameNum)
	if hdr&0x1F == 5 {
		w.ue(0) // idr_pic_id
	}
	w.u(6, lsb)
	w.u(16, 0xBEEF)
	return appendEPB([]byte{hdr}, w.rbsp())
}

// Non-IDR I keyframes get frame_num counting reference keyframes since the
// IDR and a POC LSB counting up by a quarter of its range; an IDR resets
// both and is left as is.
func TestAVCPOCRenumber(t *testing.T) {
	a := newAVCPOCRenumber(avcCRecord())
	steps := []struct {
		hdr           byte
		frameNum, lsb uint32
	}{{0x65, 0, 0}, {0x41, 1, 16}, {0x41, 2, 32}, {0x01, 3, 48}, {0x41, 3, 0}, {0x65, 0, 0}, {0x41, 1, 16}}
	for i, st := range steps {
		in := lp(testAVCSlice(st.hdr, 7, 13, 51))
		out, err := a.renumber(nil, in, 4)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		want := lp(testAVCSlice(st.hdr, 7, st.frameNum, st.lsb))
		if st.hdr == 0x65 {
			want = in
		}
		if !bytes.Equal(out, want) {
			t.Fatalf("step %d:\n got %x\nwant %x", i, out, want)
		}
	}
}
