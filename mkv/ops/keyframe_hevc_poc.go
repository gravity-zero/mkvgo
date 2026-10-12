package ops

// keyframe_hevc_poc.go - renumbering the picture order count of HEVC CRA
// keyframes decoded back to back.
//
// A CRA does not reset the picture order count: its decoder derives the POC
// MSB from the previous picture's, assuming the two are less than half the
// POC LSB range apart. Keyframes picked seconds apart are not - the gap holds
// hundreds of pictures - so a decoder fed the keyframes alone derives stale
// POCs, outputs the pictures in that order instead of decode order, and
// rejects those whose stale POC equals one still held as a reference
// (FFmpeg: "Duplicate POC in a sequence"). Measured on a CRA-keyed 2160p
// remux: 740 pictures out of 795, 380 out of place. FFmpeg's own
// `-skip_frame nokey` decode of the source has the same failure. Ending the
// sequence (EOS NAL) or relabelling the CRA as a BLA does not help in FFmpeg:
// the first makes it drop the pictures it holds, the second leaves the
// reference collisions.
//
// The fix keeps the stream as is but for slice_pic_order_cnt_lsb, rewritten
// in place (same width) on each CRA/BLA slice so successive keyframes count
// up by a quarter of the LSB range: strictly increasing POCs the decoder
// derives exactly, far enough apart that no reference picture set entry of
// one keyframe names another. An IDR resets the count. The pixels do not
// depend on the POC of an intra picture: measured identical to a full decode.

import "errors"

var errSliceHeader = errors.New("slice header names a parameter set the stream did not carry")

type hevcSPSInfo struct {
	separateColourPlane bool
	log2MaxPOCLsb       int
	sliceAddrBits       int
}

type hevcPPSInfo struct {
	sps                   uint32
	dependentSliceEnabled bool
	outputFlagPresent     bool
	extraSliceHeaderBits  int
}

// hevcPOCRenumber holds the parameter sets seen so far and the keyframe count
// since the last IDR.
type hevcPOCRenumber struct {
	sps   map[uint32]hevcSPSInfo
	pps   map[uint32]hevcPPSInfo
	count uint32
	rbsp  []byte // scratch
}

func newHEVCPOCRenumber(hvcC []byte) *hevcPOCRenumber {
	h := &hevcPOCRenumber{sps: map[uint32]hevcSPSInfo{}, pps: map[uint32]hevcPPSInfo{}}
	if len(hvcC) < 23 {
		return h
	}
	pos := 23
	for a := 0; a < int(hvcC[22]) && pos+3 <= len(hvcC); a++ {
		count := int(hvcC[pos+1])<<8 | int(hvcC[pos+2])
		pos += 3
		for i := 0; i < count && pos+2 <= len(hvcC); i++ {
			n := int(hvcC[pos])<<8 | int(hvcC[pos+1])
			pos += 2
			if pos+n > len(hvcC) {
				return h
			}
			h.learn(hvcC[pos : pos+n])
			pos += n
		}
	}
	return h
}

// learn records an SPS or PPS NAL unit (base layer only).
func (h *hevcPOCRenumber) learn(nal []byte) {
	if len(nal) < 3 || (nal[0]&1)<<5|nal[1]>>3 != 0 {
		return
	}
	h.rbsp = appendRBSP(h.rbsp[:0], nal[2:])
	switch (nal[0] >> 1) & 0x3F {
	case 33:
		if id, s, ok := parseHEVCSPSForPOC(h.rbsp); ok {
			h.sps[id] = s
		}
	case 34:
		r := expGolomb{data: h.rbsp}
		id, sps := r.ue(), r.ue()
		p := hevcPPSInfo{sps: sps, dependentSliceEnabled: r.bits(1) == 1, outputFlagPresent: r.bits(1) == 1, extraSliceHeaderBits: int(r.bits(3))}
		if !r.err {
			h.pps[id] = p
		}
	}
}

// renumber rewrites, in dst, the picture in data (NAL units length-prefixed on
// lenSize bytes) with its CRA/BLA slices renumbered, and returns dst. Every
// slice segment of the picture gets the same LSB. On error the picture is not
// emitted, so the count is left as it was: a skipped keyframe must not open a
// gap of half the LSB range or more, which the decoder would read as a wrap.
func (h *hevcPOCRenumber) renumber(dst, data []byte, lenSize int) ([]byte, error) {
	saved := h.count
	counted := false
	var failed error
	err := forEachNAL(data, lenSize, func(nal []byte) bool {
		t := (nal[0] >> 1) & 0x3F
		base := len(nal) >= 2 && (nal[0]&1)<<5|nal[1]>>3 == 0
		switch {
		case base && (t == 33 || t == 34):
			h.learn(nal)
		case base && (t == 19 || t == 20): // IDR: the POC restarts at 0
			h.count = 0
		case base && t >= 16 && t <= 21 && t != 19 && t != 20:
			if !counted {
				h.count++
				counted = true
			}
			out, err := h.rewriteSlice(nal)
			if err != nil {
				failed = err
				return false
			}
			dst = appendLengthPrefixed(dst, out, lenSize)
			return true
		}
		dst = appendLengthPrefixed(dst, nal, lenSize)
		return true
	})
	if err == nil {
		err = failed
	}
	if err != nil {
		h.count = saved
	}
	return dst, err
}

// rewriteSlice returns the slice NAL with its slice_pic_order_cnt_lsb set from
// the keyframe count (the NAL itself when the slice segment is dependent and
// carries none).
func (h *hevcPOCRenumber) rewriteSlice(nal []byte) ([]byte, error) {
	h.rbsp = appendRBSP(h.rbsp[:0], nal[2:])
	r := expGolomb{data: h.rbsp}
	first := r.bits(1) == 1
	r.bits(1) // no_output_of_prior_pics_flag (IRAP)
	pps, ok := h.pps[r.ue()]
	if !ok {
		return nil, errSliceHeader
	}
	sps, ok := h.sps[pps.sps]
	if !ok {
		return nil, errSliceHeader
	}
	if !first {
		if pps.dependentSliceEnabled && r.bits(1) == 1 {
			return nal, nil // dependent slice segment: no POC field
		}
		r.bits(sps.sliceAddrBits)
	}
	r.bits(pps.extraSliceHeaderBits)
	r.ue() // slice_type
	if pps.outputFlagPresent {
		r.bits(1)
	}
	if sps.separateColourPlane {
		r.bits(2)
	}
	n := sps.log2MaxPOCLsb
	if r.err || r.pos+n > len(h.rbsp)*8 {
		return nil, errSliceHeader
	}
	lsb := (h.count << (n - 2)) & (1<<n - 1)
	for i := 0; i < n; i++ {
		p := r.pos + i
		mask := byte(0x80) >> (p & 7)
		if lsb>>(n-1-i)&1 == 1 {
			h.rbsp[p>>3] |= mask
		} else {
			h.rbsp[p>>3] &^= mask
		}
	}
	out := make([]byte, 0, len(nal)+8)
	out = append(out, nal[0], nal[1])
	return appendEPB(out, h.rbsp), nil
}

// parseHEVCSPSForPOC reads the SPS fields a slice header up to its POC LSB
// depends on.
func parseHEVCSPSForPOC(rbsp []byte) (uint32, hevcSPSInfo, bool) {
	r := expGolomb{data: rbsp}
	r.bits(4) // sps_video_parameter_set_id
	maxSub := int(r.bits(3))
	r.bits(1)  // temporal_id_nesting
	r.bits(96) // general profile/tier/level
	profilePresent := make([]bool, maxSub)
	levelPresent := make([]bool, maxSub)
	for i := 0; i < maxSub; i++ {
		profilePresent[i], levelPresent[i] = r.bits(1) == 1, r.bits(1) == 1
	}
	if maxSub > 0 {
		r.bits(2 * (8 - maxSub))
	}
	for i := 0; i < maxSub; i++ {
		if profilePresent[i] {
			r.bits(88)
		}
		if levelPresent[i] {
			r.bits(8)
		}
	}
	id := r.ue()
	var s hevcSPSInfo
	if r.ue() == 3 { // chroma_format_idc
		s.separateColourPlane = r.bits(1) == 1
	}
	w, h := r.ue(), r.ue()
	if r.bits(1) == 1 { // conformance window
		r.ue()
		r.ue()
		r.ue()
		r.ue()
	}
	r.ue() // bit_depth_luma_minus8
	r.ue() // bit_depth_chroma_minus8
	s.log2MaxPOCLsb = int(r.ue()) + 4
	all := r.bits(1) == 1 // sps_sub_layer_ordering_info_present_flag
	from := maxSub
	if all {
		from = 0
	}
	for i := from; i <= maxSub; i++ {
		r.ue()
		r.ue()
		r.ue()
	}
	minCb := int(r.ue()) + 3
	ctb := minCb + int(r.ue())
	if r.err || s.log2MaxPOCLsb > 16 || ctb > 7 || w == 0 || h == 0 {
		return 0, s, false
	}
	size := uint32(1) << ctb
	ctbs := ((w + size - 1) / size) * ((h + size - 1) / size)
	for ctbs > 1<<s.sliceAddrBits {
		s.sliceAddrBits++
	}
	return id, s, true
}

// appendRBSP appends a NAL payload to dst with its emulation prevention bytes
// removed.
func appendRBSP(dst, b []byte) []byte {
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		dst = append(dst, c)
	}
	return dst
}

// appendEPB appends an RBSP to dst with emulation prevention bytes inserted.
func appendEPB(dst, rbsp []byte) []byte {
	zeros := 0
	for _, c := range rbsp {
		if zeros >= 2 && c <= 3 {
			dst = append(dst, 3)
			zeros = 0
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		dst = append(dst, c)
	}
	if zeros > 0 {
		dst = append(dst, 3) // a NAL unit never ends on a zero byte
	}
	return dst
}

func appendLengthPrefixed(dst, nal []byte, lenSize int) []byte {
	for i := lenSize - 1; i >= 0; i-- {
		dst = append(dst, byte(len(nal)>>(8*i)))
	}
	return append(dst, nal...)
}
