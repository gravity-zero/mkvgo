package ops

// keyframe_avc_poc.go - the H.264 counterpart of keyframe_hevc_poc.go, for
// open-GOP streams whose keyframes are non-IDR I pictures.
//
// Decoded back to back, such keyframes break two counters the decoder keeps
// across pictures: frame_num jumps by the hundreds of pictures skipped in
// between, which a decoder treats as lost frames and fills (FFmpeg outputs
// duplicates), and pic_order_cnt_lsb wraps unseen, so the picture order is
// derived wrong (pictures out of order, some never output). Measured on an
// x264 open-GOP clip: 15 pictures out for 18 keyframes, duplicates included.
//
// Both fields are rewritten in place, same width: frame_num counts the
// reference keyframes since the last IDR (no gap), and with POC type 0 the
// LSB counts up by a quarter of its range (types 1 and 2 derive the POC from
// frame_num). Field-coded pictures are not renumbered (an error: the sample is
// skipped). IDR pictures reset both counters and are left untouched.

type avcSPSInfo struct {
	separateColourPlane bool
	log2MaxFrameNum     int
	pocType             uint32
	log2MaxPOCLsb       int
	frameMbsOnly        bool
}

type avcPOCRenumber struct {
	sps  map[uint32]avcSPSInfo
	pps  map[uint32]uint32 // pps id -> sps id
	refs uint32            // reference keyframes since the last IDR
	pics uint32            // keyframes since the last IDR
	rbsp []byte
}

func newAVCPOCRenumber(avcC []byte) *avcPOCRenumber {
	a := &avcPOCRenumber{sps: map[uint32]avcSPSInfo{}, pps: map[uint32]uint32{}}
	if len(avcC) < 6 {
		return a
	}
	pos := 6
	read := func(count int) {
		for i := 0; i < count && pos+2 <= len(avcC); i++ {
			n := int(avcC[pos])<<8 | int(avcC[pos+1])
			pos += 2
			if pos+n > len(avcC) {
				pos = len(avcC)
				return
			}
			a.learn(avcC[pos : pos+n])
			pos += n
		}
	}
	read(int(avcC[5] & 0x1F))
	if pos < len(avcC) {
		count := int(avcC[pos])
		pos++
		read(count)
	}
	return a
}

func (a *avcPOCRenumber) learn(nal []byte) {
	if len(nal) < 2 {
		return
	}
	a.rbsp = appendRBSP(a.rbsp[:0], nal[1:])
	switch nal[0] & 0x1F {
	case 7:
		if id, s, ok := parseAVCSPSForPOC(a.rbsp); ok {
			a.sps[id] = s
		}
	case 8:
		r := expGolomb{data: a.rbsp}
		id, sps := r.ue(), r.ue()
		if !r.err {
			a.pps[id] = sps
		}
	}
}

// renumber rewrites, in dst, the picture in data (length-prefixed NAL units)
// with its non-IDR slices renumbered, and returns dst. Every slice of the
// picture gets the same values. On error the picture is not emitted, so the
// counters are left as they were: a skipped reference keyframe must not leave
// a frame_num gap.
func (a *avcPOCRenumber) renumber(dst, data []byte, lenSize int) ([]byte, error) {
	savedRefs, savedPics := a.refs, a.pics
	counted := false
	var frameNum, lsb uint32
	var failed error
	err := forEachNAL(data, lenSize, func(nal []byte) bool {
		switch nal[0] & 0x1F {
		case 7, 8:
			a.learn(nal)
		case 5:
			a.refs, a.pics = 0, 0
		case 1:
			if !counted {
				counted = true
				a.pics++
				frameNum = a.refs + 1
				if nal[0]&0x60 != 0 { // nal_ref_idc: a reference picture
					a.refs++
				}
				lsb = a.pics
			}
			out, err := a.rewriteSlice(nal, frameNum, lsb)
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
		a.refs, a.pics = savedRefs, savedPics
	}
	return dst, err
}

func (a *avcPOCRenumber) rewriteSlice(nal []byte, frameNum, pic uint32) ([]byte, error) {
	a.rbsp = appendRBSP(a.rbsp[:0], nal[1:])
	r := expGolomb{data: a.rbsp}
	r.ue() // first_mb_in_slice
	r.ue() // slice_type
	spsID, ok := a.pps[r.ue()]
	if !ok {
		return nil, errSliceHeader
	}
	sps, ok := a.sps[spsID]
	if !ok {
		return nil, errSliceHeader
	}
	if sps.separateColourPlane {
		r.bits(2)
	}
	setBits := func(pos, n int, v uint32) {
		for i := 0; i < n; i++ {
			p := pos + i
			mask := byte(0x80) >> (p & 7)
			if v>>(n-1-i)&1 == 1 {
				a.rbsp[p>>3] |= mask
			} else {
				a.rbsp[p>>3] &^= mask
			}
		}
	}
	fnPos := r.pos
	r.bits(sps.log2MaxFrameNum)
	if !sps.frameMbsOnly && r.bits(1) == 1 {
		return nil, errNotAccessPoint // field-coded picture: not renumbered
	}
	if r.err {
		return nil, errSliceHeader
	}
	setBits(fnPos, sps.log2MaxFrameNum, frameNum&(1<<sps.log2MaxFrameNum-1))
	if sps.pocType == 0 {
		n := sps.log2MaxPOCLsb
		if r.pos+n > len(a.rbsp)*8 {
			return nil, errSliceHeader
		}
		setBits(r.pos, n, (pic<<(n-2))&(1<<n-1))
	}
	out := make([]byte, 0, len(nal)+8)
	out = append(out, nal[0])
	return appendEPB(out, a.rbsp), nil
}

// parseAVCSPSForPOC reads the SPS fields a slice header up to its POC LSB
// depends on.
func parseAVCSPSForPOC(rbsp []byte) (uint32, avcSPSInfo, bool) {
	r := expGolomb{data: rbsp}
	profile := r.bits(8)
	r.bits(16) // constraint flags, level
	id := r.ue()
	var s avcSPSInfo
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma := r.ue()
		if chroma == 3 {
			s.separateColourPlane = r.bits(1) == 1
		}
		r.ue() // bit_depth_luma_minus8
		r.ue() // bit_depth_chroma_minus8
		r.bits(1)
		if r.bits(1) == 1 { // seq_scaling_matrix_present_flag
			lists := 8
			if chroma == 3 {
				lists = 12
			}
			for i := 0; i < lists; i++ {
				if r.bits(1) == 0 {
					continue
				}
				size := 16
				if i >= 6 {
					size = 64
				}
				last, next := int32(8), int32(8)
				for j := 0; j < size && !r.err; j++ {
					if next != 0 {
						next = (last + r.se() + 256) % 256
					}
					if next != 0 {
						last = next
					}
				}
			}
		}
	}
	s.log2MaxFrameNum = int(r.ue()) + 4
	s.pocType = r.ue()
	switch s.pocType {
	case 0:
		s.log2MaxPOCLsb = int(r.ue()) + 4
	case 1:
		r.bits(1)
		r.se()
		r.se()
		n := r.ue()
		for i := uint32(0); i < n && !r.err; i++ {
			r.se()
		}
	}
	r.ue()    // max_num_ref_frames
	r.bits(1) // gaps_in_frame_num_value_allowed_flag
	r.ue()    // pic_width_in_mbs_minus1
	r.ue()    // pic_height_in_map_units_minus1
	s.frameMbsOnly = r.bits(1) == 1
	if r.err || s.log2MaxFrameNum > 16 || s.log2MaxPOCLsb > 16 || s.pocType > 2 {
		return 0, s, false
	}
	return id, s, true
}
