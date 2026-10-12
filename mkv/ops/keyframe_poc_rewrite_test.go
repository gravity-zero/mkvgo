package ops

import (
	"bytes"
	"testing"
)

// The POC/frame_num rewrite works on the RBSP (emulation prevention bytes
// removed), sets the bits there, then escapes the whole NAL payload again. A
// changed bit can create a 00 00 0x run that needs a new 0x03, or break one
// whose 0x03 must go: these tests pin both, multi-slice pictures, and the
// failure path (picture skipped, counters untouched).

// canonicalNAL fails when nal (header excluded by skip) holds a start code
// emulation or a 0x03 that does not escape one.
func canonicalNAL(t *testing.T, nal []byte, skip int) {
	t.Helper()
	body := nal[skip:]
	for i := 2; i < len(body); i++ {
		if body[i-2] == 0 && body[i-1] == 0 && body[i] <= 2 {
			t.Fatalf("start code emulation at %d in %x", i, nal)
		}
	}
	if !bytes.Equal(appendEPB(nil, appendRBSP(nil, body)), body) {
		t.Fatalf("not canonically escaped: %x", nal)
	}
}

func nals(t *testing.T, sample []byte) [][]byte {
	t.Helper()
	var out [][]byte
	if err := forEachNAL(sample, 4, func(nal []byte) bool {
		out = append(out, nal)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func hvccOf(ps ...[]byte) []byte {
	rec := make([]byte, 23)
	rec[0] = 1
	rec[21] = 0x03
	rec[22] = byte(len(ps))
	for _, nal := range ps {
		rec = append(rec, 0x80|(nal[0]>>1), 0, 1, byte(len(nal)>>8), byte(len(nal)))
		rec = append(rec, nal...)
	}
	return rec
}

// hevcPPSWith is PPS 0 on SPS 0 (the 64x48 testHEVCSPS: 12 CTBs, 4-bit slice
// addresses, 8-bit POC LSB).
func hevcPPSWith(dependent bool, extraBits int) []byte {
	var w bitWriter
	w.ue(0)
	w.ue(0)
	b := uint32(0)
	if dependent {
		b = 1
	}
	w.u(1, b)
	w.u(1, 0)
	w.u(3, uint32(extraBits))
	w.u(8, 0x5A)
	return hevcNAL(34, w.rbsp())
}

type hevcSeg struct {
	typ        byte
	first, dep bool // dep: dependent slice segment (PPS must enable them)
	depEnabled bool
	addr       uint32
	extraBits  int
	lsb        uint32
	tail       []byte
	cabacZeros int // cabac_zero_words (0x0000) after the trailing bits
}

func (s hevcSeg) nal() []byte {
	var w bitWriter
	if s.first {
		w.u(1, 1)
	} else {
		w.u(1, 0)
	}
	w.u(1, 0) // no_output_of_prior_pics_flag
	w.ue(0)
	if !s.first {
		if s.depEnabled {
			if s.dep {
				w.u(1, 1)
			} else {
				w.u(1, 0)
			}
		}
		w.u(4, s.addr)
	}
	if !s.dep {
		w.u(s.extraBits, 0)
		w.ue(2) // I slice
		if s.typ != 19 && s.typ != 20 {
			w.u(8, s.lsb)
		}
	}
	for _, b := range s.tail {
		w.u(8, uint32(b))
	}
	rbsp := append(w.rbsp(), make([]byte, 2*s.cabacZeros)...)
	return hevcNAL(s.typ, rbsp)
}

func (s hevcSeg) with(lsb uint32) hevcSeg { s.lsb = lsb; return s }

// (a) emulation prevention, HEVC. With 2 extra slice header bits the 8-bit
// LSB of a first slice segment fills byte 1 of the payload, so the rewrite
// alone decides whether that byte is zero.
func TestHEVCPOCRewriteEmulationPrevention(t *testing.T) {
	cases := []struct {
		name          string
		count         uint32 // keyframes counted before this one
		in            hevcSeg
		wantLSB       uint32
		wantLenChange int
	}{
		// 0xC0 -> 0x00 before 00 01: the rewrite creates 00 00 01, a 0x03 goes in.
		{"creates", 3, hevcSeg{typ: 21, first: true, extraBits: 2, lsb: 0xC0, tail: []byte{0x00, 0x01, 0x7F}}, 0x00, +1},
		// 0x00 before 00 02 (escaped at the source) -> 0x40: the 0x03 goes away.
		{"destroys", 0, hevcSeg{typ: 21, first: true, extraBits: 2, lsb: 0x00, tail: []byte{0x00, 0x02, 0x7F}}, 0x40, -1},
		// The LSB becomes zero next to slice data already escaped, and
		// cabac_zero_words close the NAL (escaped up to the final 0x03).
		{"creates next to escaped data", 3, hevcSeg{typ: 16, first: true, extraBits: 2, lsb: 0x80, tail: []byte{0x00, 0x00, 0x00, 0x03, 0x00, 0x00, 0x01}, cabacZeros: 2}, 0x00, +1},
		// Nothing to escape either way: same length.
		{"neutral", 0, hevcSeg{typ: 21, first: true, extraBits: 2, lsb: 0xFF, tail: []byte{0x11}}, 0x40, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHEVCPOCRenumber(hvccOf(testHEVCSPS(), hevcPPSWith(false, 2)))
			h.count = c.count
			in := c.in.nal()
			out, err := h.renumber(nil, lp(in), 4)
			if err != nil {
				t.Fatal(err)
			}
			got := nals(t, out)[0]
			want := c.in.with(c.wantLSB).nal()
			if !bytes.Equal(got, want) {
				t.Fatalf("\n  in %x\n got %x\nwant %x", in, got, want)
			}
			if d := len(got) - len(in); d != c.wantLenChange {
				t.Fatalf("length change %d, want %d (in %x, out %x)", d, c.wantLenChange, in, got)
			}
			canonicalNAL(t, got, 2)
		})
	}
}

// (b) multi-slice HEVC picture: every independent slice segment gets the same
// LSB, a dependent one (no POC field) is copied as is, the count moves once.
func TestHEVCPOCRewriteMultiSlice(t *testing.T) {
	h := newHEVCPOCRenumber(hvccOf(testHEVCSPS(), hevcPPSWith(true, 0)))
	segs := []hevcSeg{
		{typ: 21, first: true, depEnabled: true, lsb: 0x11, tail: []byte{0xA1}},
		{typ: 21, depEnabled: true, addr: 4, lsb: 0x11, tail: []byte{0x00, 0x00, 0x00}},
		{typ: 21, depEnabled: true, dep: true, addr: 6, tail: []byte{0xB2}},
		{typ: 21, depEnabled: true, addr: 8, lsb: 0x11, tail: []byte{0xC3}},
	}
	for pic, wantLSB := range []uint32{0x40, 0x80} {
		var in [][]byte
		for _, s := range segs {
			in = append(in, s.nal())
		}
		out, err := h.renumber(nil, lp(in...), 4)
		if err != nil {
			t.Fatal(err)
		}
		got := nals(t, out)
		if len(got) != len(segs) {
			t.Fatalf("picture %d: %d NAL units out of %d", pic, len(got), len(segs))
		}
		for i, s := range segs {
			want := s.with(wantLSB).nal()
			if s.dep {
				want = in[i]
			}
			if !bytes.Equal(got[i], want) {
				t.Fatalf("picture %d slice %d:\n got %x\nwant %x", pic, i, got[i], want)
			}
			canonicalNAL(t, got[i], 2)
		}
	}
}

// A picture with one slice the rewrite cannot read is an error (the caller
// skips the sample) and leaves the count where it was: the next keyframe gets
// the next quarter step, not two. Skipping three in a row would otherwise
// jump three quarters, which the decoder reads as a backward wrap.
func TestHEVCPOCRewriteFailedPictureKeepsCount(t *testing.T) {
	h := newHEVCPOCRenumber(hvccOf(testHEVCSPS(), testHEVCPPS()))
	good := hevcSeg{typ: 21, first: true, lsb: 0x33, tail: []byte{0x44}}
	// Second slice names PPS 5, never seen.
	var w bitWriter
	w.u(1, 0)
	w.u(1, 0)
	w.ue(5)
	w.u(4, 6)
	w.ue(2)
	w.u(8, 0x33)
	bad := hevcNAL(21, w.rbsp())
	for i := 0; i < 3; i++ {
		if _, err := h.renumber(nil, lp(good.nal(), bad), 4); err == nil {
			t.Fatal("picture with an unreadable slice renumbered")
		}
	}
	out, err := h.renumber(nil, lp(good.nal()), 4)
	if err != nil {
		t.Fatal(err)
	}
	if got := sliceLSB(t, out); got != 0x40 {
		t.Fatalf("LSB %#x after 3 skipped pictures, want 0x40", got)
	}
}

type avcParams struct {
	fnBits, lsbBits int
	frameMbsOnly    bool
}

func (p avcParams) sps() []byte {
	var w bitWriter
	w.u(8, 66)
	w.u(8, 0)
	w.u(8, 30)
	w.ue(0)
	w.ue(uint32(p.fnBits - 4))
	w.ue(0)
	w.ue(uint32(p.lsbBits - 4))
	w.ue(1)
	w.u(1, 0)
	w.ue(19)
	w.ue(14)
	if p.frameMbsOnly {
		w.u(1, 1)
	} else {
		w.u(1, 0)
		w.u(1, 0) // mb_adaptive_frame_field_flag
	}
	w.u(1, 1)
	return appendEPB([]byte{0x67}, w.rbsp())
}

func (p avcParams) record() []byte {
	sps := p.sps()
	c := []byte{1, 66, 0, 30, 0xFF, 0xE1, 0, byte(len(sps))}
	c = append(c, sps...)
	pps := testAVCPPS()
	c = append(c, 1, 0, byte(len(pps)))
	return append(c, pps...)
}

// slice is a non-IDR I slice (nal_ref_idc 2) starting at macroblock firstMb.
func (p avcParams) slice(firstMb, frameNum, lsb uint32, field bool, tail []byte) []byte {
	var w bitWriter
	w.ue(firstMb)
	w.ue(7)
	w.ue(0)
	w.u(p.fnBits, frameNum)
	if !p.frameMbsOnly {
		if field {
			w.u(1, 1)
			w.u(1, 0) // bottom_field_flag
		} else {
			w.u(1, 0)
		}
	}
	w.u(p.lsbBits, lsb)
	for _, b := range tail {
		w.u(8, uint32(b))
	}
	return appendEPB([]byte{0x41}, w.rbsp())
}

// (a) emulation prevention, H.264. 16-bit frame_num and POC LSB: renumbered
// values are mostly zero bits (frame_num 1, LSB 0x4000), source values are
// chosen all ones or all zeros, so the rewrite creates and breaks start code
// emulations. Each case checks the exact bytes, the canonical escaping and
// the direction of the length change.
func TestAVCPOCRewriteEmulationPrevention(t *testing.T) {
	p := avcParams{fnBits: 16, lsbBits: 16, frameMbsOnly: true}
	cases := []struct {
		name          string
		inFN, inLSB   uint32
		tail          []byte
		wantLenChange int
	}{
		{"creates", 0xFFFF, 0xFFFF, []byte{0x00, 0x01, 0x55}, +1},
		{"destroys", 0x0000, 0x0000, []byte{0x00, 0x02, 0x55}, -1},
		{"neutral", 0x1234, 0x4000, []byte{0x55}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newAVCPOCRenumber(p.record())
			in := p.slice(0, c.inFN, c.inLSB, false, c.tail)
			out, err := a.renumber(nil, lp(in), 4)
			if err != nil {
				t.Fatal(err)
			}
			got := nals(t, out)[0]
			want := p.slice(0, 1, 0x4000, false, c.tail)
			if !bytes.Equal(got, want) {
				t.Fatalf("\n  in %x\n got %x\nwant %x", in, got, want)
			}
			if d := len(got) - len(in); d != c.wantLenChange {
				t.Fatalf("length change %d, want %d (in %x, out %x)", d, c.wantLenChange, in, got)
			}
			canonicalNAL(t, got, 1)
		})
	}
}

// (b) multi-slice H.264 picture: every slice gets the same frame_num and
// LSB, the counters move once per picture.
func TestAVCPOCRewriteMultiSlice(t *testing.T) {
	p := avcParams{fnBits: 4, lsbBits: 6, frameMbsOnly: true}
	a := newAVCPOCRenumber(p.record())
	firsts := []uint32{0, 100, 200}
	for pic, want := range []struct{ fn, lsb uint32 }{{1, 16}, {2, 32}, {3, 48}} {
		var in [][]byte
		for _, mb := range firsts {
			in = append(in, p.slice(mb, 9, 5, false, []byte{0x00, 0x00, 0x00}))
		}
		out, err := a.renumber(nil, lp(in...), 4)
		if err != nil {
			t.Fatal(err)
		}
		got := nals(t, out)
		if len(got) != len(firsts) {
			t.Fatalf("picture %d: %d slices out", pic, len(got))
		}
		for i, mb := range firsts {
			if w := p.slice(mb, want.fn, want.lsb, false, []byte{0x00, 0x00, 0x00}); !bytes.Equal(got[i], w) {
				t.Fatalf("picture %d slice %d:\n got %x\nwant %x", pic, i, got[i], w)
			}
			canonicalNAL(t, got[i], 1)
		}
	}
}

// A field-coded slice anywhere in the picture (here the second) makes it an
// error, not a half-renumbered picture, and the counters stay put: the next
// keyframe gets frame_num 1, no gap the decoder would fill.
func TestAVCPOCRewriteFieldSliceSkippedKeepsCounters(t *testing.T) {
	p := avcParams{fnBits: 4, lsbBits: 6, frameMbsOnly: false}
	a := newAVCPOCRenumber(p.record())
	frame := p.slice(0, 9, 5, false, []byte{0x77})
	field := p.slice(100, 9, 5, true, []byte{0x77})
	if _, err := a.renumber(nil, lp(frame, field), 4); err == nil {
		t.Fatal("picture with a field slice renumbered")
	}
	out, err := a.renumber(nil, lp(frame), 4)
	if err != nil {
		t.Fatal(err)
	}
	if want := lp(p.slice(0, 1, 16, false, []byte{0x77})); !bytes.Equal(out, want) {
		t.Fatalf("\n got %x\nwant %x", out, want)
	}
}
