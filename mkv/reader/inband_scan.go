package reader

import "github.com/gravity-zero/mkvgo/mkv"

// maxScanSamples bounds how many samples of an H.264 track are read looking for
// proof of interlacing: an encoder that codes some pictures as frames rarely codes
// that many in a row.
const maxScanSamples = 16

// maxSliceHeader is how much of a slice NAL is unescaped to reach the picture
// order count fields: a slice header gets there in far fewer bytes.
const maxSliceHeader = 64

// avcSPSInfo is what H.264 slice headers and picture timing SEI need from an SPS.
type avcSPSInfo struct {
	id              uint32
	fieldCoding     bool // frame_mbs_only_flag 0: slices carry field_pic_flag
	mbaff           bool // mb_adaptive_frame_field_flag
	separatePlanes  bool // slices carry colour_plane_id
	frameNumBits    int
	pocType         int
	pocLsbBits      int   // pic_order_cnt_type 0
	deltaAlwaysZero bool  // pic_order_cnt_type 1
	offsetTopBottom int32 // pic_order_cnt_type 1: offset_for_top_to_bottom_field
	cpbDpbDelays    bool  // an HRD is present: pic timing opens on the two delays
	cpbLen, dpbLen  int
	picStruct       bool // pic_struct_present_flag
}

// valid reports whether the field widths a slice header is read with are in range.
func (s *avcSPSInfo) valid() bool {
	return s.frameNumBits >= 4 && s.frameNumBits <= 16 && s.pocType <= 2 &&
		(s.pocType != 0 || (s.pocLsbBits >= 4 && s.pocLsbBits <= 16))
}

type avcPPSInfo struct {
	spsID     uint32
	bottomPOC bool // bottom_field_pic_order_in_frame_present_flag
}

// avcScan gathers, over the first samples of an H.264 track whose SPS allows
// field coding, the proof that the track is interlaced and the field order the
// stream states. Only what the bitstream proves is kept: a frame-coded picture
// whose fields share one instant proves nothing either way.
type avcScan struct {
	nalLen int
	sps    map[uint32]*avcSPSInfo
	pps    map[uint32]avcPPSInfo

	// The record holds no SPS (avc3): the samples carry it, and an in-band SPS
	// that codes frames only proves the track progressive, as it would head-only.
	inBandSPS             bool
	sawSPS, sawFieldCoded bool

	interlaced bool
	// Field orders the samples stated. pic_struct is the display order and wins,
	// as in the reference prober's parser: on an x264 stream whose frames change
	// field order the two disagree, the POC keeping one order throughout.
	structOrders map[string]bool
	pocOrders    map[string]bool
	pending      *avcFieldPic // a field still waiting for the other of its pair

	// pic_struct "top, bottom" / "bottom, top" on a frame is interlaced only
	// outside soft telecine, which repeats fields (pic_struct 5/6) or marks
	// its frames progressive (ct_type 0); settled once all samples are read.
	pairStruct  string
	pulldown    bool
	progressive bool
}

type avcFieldPic struct {
	frameNum uint32
	bottom   bool
	lsb      uint32
	pocType  int
	lsbBits  int
}

// newAVCScan reads the parameter sets of an avcC; nil when the record is
// malformed or its SPS all code frames only.
func newAVCScan(cp []byte) *avcScan {
	if len(cp) < 7 || cp[0] != 1 {
		return nil
	}
	s := &avcScan{nalLen: int(cp[4]&0x03) + 1, sps: map[uint32]*avcSPSInfo{}, pps: map[uint32]avcPPSInfo{}, structOrders: map[string]bool{}, pocOrders: map[string]bool{}}
	field, hasSPS := false, false
	if !forEachAVCSPS(cp, func(bc *bitstreamColour) bool {
		s.addSPS(bc)
		field = field || bc.avc.fieldCoding
		hasSPS = true
		return false
	}) || (hasSPS && !field) {
		return nil
	}
	s.inBandSPS = cp[5]&0x1f == 0 && !hasSPS
	for _, pps := range avcCPPSList(cp) {
		s.addPPS(pps)
	}
	return s
}

func (s *avcScan) addSPS(bc *bitstreamColour) {
	if info := bc.avc; info.valid() {
		s.sps[info.id] = &info
	}
}

// addPPS reads the head of a PPS NAL (header byte included).
func (s *avcScan) addPPS(nal []byte) {
	if len(nal) < 2 {
		return
	}
	r := &bitReader{data: unescapeRBSP(nal[1:min(len(nal), maxSliceHeader)])}
	id := r.ue()
	p := avcPPSInfo{spsID: r.ue()}
	r.bit() // entropy_coding_mode_flag
	p.bottomPOC = r.bit() == 1
	if !r.err {
		s.pps[id] = p
	}
}

// avcCPPSList returns the PPS NALs that follow the SPS list of an avcC.
func avcCPPSList(cp []byte) [][]byte {
	off := 6
	for i := 0; i < int(cp[5]&0x1f); i++ {
		if off+2 > len(cp) {
			return nil
		}
		off += 2 + (int(cp[off])<<8 | int(cp[off+1]))
	}
	if off >= len(cp) {
		return nil
	}
	n := int(cp[off])
	off++
	var out [][]byte
	for i := 0; i < n; i++ {
		if off+2 > len(cp) {
			return out
		}
		l := int(cp[off])<<8 | int(cp[off+1])
		off += 2
		if l < 1 || off+l > len(cp) {
			return out
		}
		out = append(out, cp[off:off+l])
		off += l
	}
	return out
}

// done reports whether more samples cannot change the verdict: interlaced, and
// display orders that already disagree. One order is checked against every sample.
func (s *avcScan) done() bool { return s.interlaced && len(s.structOrders) > 1 }

// feed reads one length-prefixed access unit: in-band parameter sets, the picture
// timing SEI, and the first slice of each picture in it. truncated says frame is
// only the first bytes of the sample: its last NAL may then be cut short, and
// the head of a cut slice is still read.
func (s *avcScan) feed(frame []byte, truncated bool) {
	defer func() { _ = recover() }() // the walkers bound every length; a frame they misread must not end the read
	s.walk(frame, truncated)
}

// walk is feed without the backstop, which the fuzz target calls so a missing
// bound panics rather than hides.
func (s *avcScan) walk(frame []byte, truncated bool) {
	var seis [][]byte
	var picSPS *avcSPSInfo
	forEachLengthPrefixed(frame, s.nalLen, truncated, func(nal []byte) {
		switch nal[0] & 0x1f {
		case 7:
			if bc := parseAVCSPS(unescapeRBSP(nal[1:])); bc != nil {
				s.addSPS(bc)
				s.sawSPS = true
				s.sawFieldCoded = s.sawFieldCoded || bc.avc.fieldCoding
			}
		case 8:
			s.addPPS(nal)
		case 6:
			seis = append(seis, nal)
		case 1, 5:
			if sps := s.slice(nal); sps != nil && picSPS == nil {
				picSPS = sps
			}
		}
	})
	if picSPS != nil && picSPS.picStruct {
		for _, sei := range seis {
			forEachSEIMessage(unescapeRBSP(sei[1:]), func(pt int, payload []byte) {
				if pt == 1 { // pic_timing
					s.picTiming(payload, picSPS)
				}
			})
		}
	}
}

// slice reads the header of a picture's first slice and records what it proves;
// it returns the SPS the slice uses, nil when unreadable or not a first slice.
func (s *avcScan) slice(nal []byte) *avcSPSInfo {
	r := &bitReader{data: unescapeRBSP(nal[1:min(len(nal), maxSliceHeader)])}
	if r.ue() != 0 { // first_mb_in_slice: the rest of the picture says the same
		return nil
	}
	r.ue() // slice_type
	pps, ok := s.pps[r.ue()]
	if !ok {
		return nil
	}
	sps, ok := s.sps[pps.spsID]
	if !ok {
		return nil
	}
	if sps.separatePlanes {
		r.bits(2) // colour_plane_id
	}
	frameNum := r.bits(sps.frameNumBits)
	field, bottom := false, false
	if sps.fieldCoding {
		if field = r.bit() == 1; field {
			bottom = r.bit() == 1
		}
	}
	if nal[0]&0x1f == 5 {
		r.ue() // idr_pic_id
	}
	var lsb uint32
	var delta int64 // BottomFieldOrderCnt - TopFieldOrderCnt of a frame
	switch sps.pocType {
	case 0:
		lsb = r.bits(sps.pocLsbBits)
		if pps.bottomPOC && !field {
			delta = int64(r.se())
		}
	case 1:
		delta = int64(sps.offsetTopBottom)
		if !sps.deltaAlwaysZero {
			r.se() // delta_pic_order_cnt[0]
			if pps.bottomPOC && !field {
				delta += int64(r.se())
			}
		}
	}
	if r.err {
		return nil
	}
	switch {
	case field:
		s.interlaced = true
		s.fieldPic(avcFieldPic{frameNum: frameNum, bottom: bottom, lsb: lsb, pocType: sps.pocType, lsbBits: sps.pocLsbBits})
	case delta != 0: // the two fields of the frame are sampled at different instants
		s.interlaced = true
		s.pocOrders[orderOf(delta > 0)] = true
		s.pending = nil
	default:
		s.pending = nil
	}
	return sps
}

// fieldPic pairs a field with the previous one of the same frame and records the
// order their picture order counts state - or, with pic_order_cnt_type 2, their
// decoding order, which is then the output order.
func (s *avcScan) fieldPic(f avcFieldPic) {
	p := s.pending
	if p == nil || p.frameNum != f.frameNum || p.bottom == f.bottom {
		s.pending = &f
		return
	}
	s.pending = nil
	top, bot := p, &f
	if p.bottom {
		top, bot = &f, p
	}
	switch f.pocType {
	case 0:
		m := int64(1) << top.lsbBits
		d := (int64(bot.lsb) - int64(top.lsb)) & (m - 1)
		if d > m/2 {
			d -= m
		}
		if d != 0 {
			s.pocOrders[orderOf(d > 0)] = true
		}
	case 2:
		s.pocOrders[orderOf(!p.bottom)] = true
	}
}

// picTiming reads pic_struct and the first ct_type of a picture timing SEI.
func (s *avcScan) picTiming(payload []byte, sps *avcSPSInfo) {
	r := &bitReader{data: payload}
	if sps.cpbDpbDelays {
		r.bits(sps.cpbLen)
		r.bits(sps.dpbLen)
	}
	ps := r.bits(4)
	if r.err || ps > 8 {
		return
	}
	ctType := -1
	if r.bit() == 1 { // clock_timestamp_flag of the first timestamp
		ctType = int(r.bits(2))
	}
	if r.err {
		ctType = -1
	}
	switch ps {
	case 1, 2: // a field shown alone
		s.interlaced = true
	case 3, 4:
		if ctType == 1 || sps.mbaff {
			s.pairStruct = orderOf(ps == 3)
		}
	case 5, 6:
		s.pulldown = true
	}
	switch ctType {
	case 0:
		s.progressive = true
	case 1:
		s.interlaced = true
	}
	if s.interlaced && (ps == 3 || ps == 4) {
		s.structOrders[orderOf(ps == 3)] = true
	}
}

func orderOf(topFirst bool) string {
	if topFirst {
		return "tt"
	}
	return "bb"
}

// apply writes the verdict: ScanType "interlaced" on proof, and the field order
// when every sample that stated one agreed - the pic_struct orders when there
// are any, else the picture order counts. Nothing is written otherwise.
func (s *avcScan) apply(t *mkv.Track) {
	if s.pairStruct != "" && !s.pulldown && !s.progressive {
		s.interlaced = true
		s.structOrders[s.pairStruct] = true
	}
	if t.ScanType != "" {
		return
	}
	if !s.interlaced {
		if s.inBandSPS && s.sawSPS && !s.sawFieldCoded {
			t.ScanType, t.FieldOrder = "progressive", "progressive"
		}
		return
	}
	t.ScanType = "interlaced"
	orders := s.structOrders
	if len(orders) == 0 {
		orders = s.pocOrders
	}
	if len(orders) == 1 && t.FieldOrder == "" {
		for o := range orders {
			t.FieldOrder = o
		}
	}
}

// forEachLengthPrefixed walks the NAL units of a length-prefixed access unit
// (prefix nalLen bytes, big-endian). A length past the end ends the walk, unless
// truncated: the cut NAL is then passed as far as it goes, and the walk ends.
func forEachLengthPrefixed(frame []byte, nalLen int, truncated bool, fn func(nal []byte)) {
	if nalLen < 1 || nalLen > 4 {
		return
	}
	for off := 0; off+nalLen <= len(frame); {
		n := int64(0)
		for i := 0; i < nalLen; i++ {
			n = n<<8 | int64(frame[off+i])
		}
		off += nalLen
		if n < 1 {
			return
		}
		if n > int64(len(frame)-off) {
			if truncated && off < len(frame) {
				fn(frame[off:])
			}
			return
		}
		fn(frame[off : off+int(n)])
		off += int(n)
	}
}

// InBandScanSamples is how many samples of a track an InBandScan reads at most.
const InBandScanSamples = maxScanSamples

// scanner settles a track's scan type over its first samples.
type scanner interface {
	feed(frame []byte, truncated bool)
	done() bool
	apply(t *mkv.Track)
}

// newScanner returns the scanner for t, nil when its samples have nothing to
// settle: a scan type already stated, a codec without one, or a record that
// already rules interlacing out.
func newScanner(t *mkv.Track) scanner {
	if t.Type != mkv.VideoTrack || t.ScanType != "" {
		return nil
	}
	switch {
	case isAVCCodec(t.Codec):
		if s := newAVCScan(t.CodecPrivate); s != nil {
			return s
		}
	case isHEVCCodec(t.Codec):
		if s := newHEVCScan(t.CodecPrivate); s != nil {
			return s
		}
	case t.Codec == "V_MPEG2" || t.Codec == "mpeg2":
		return &mpeg2Scan{orders: map[string]bool{}}
	}
	return nil
}

// mpeg2Scan settles an MPEG-2 track's scan type from its first samples, where
// muxers that write no CodecPrivate keep the sequence header: a sequence
// extension with progressive_sequence 1 proves every frame progressive; a field
// picture, or a frame with progressive_frame 0, proves the stream interlaced,
// top_field_first or the first field naming the order. A progressive frame
// repeating a field (soft telecine) proves nothing.
type mpeg2Scan struct {
	interlaced, progressiveSeq bool
	orders                     map[string]bool
	firstField                 bool // the last picture was the first field of a frame
}

func (s *mpeg2Scan) feed(frame []byte, _ bool) {
	for i := 0; i+5 <= len(frame); i++ {
		if frame[i] != 0 || frame[i+1] != 0 || frame[i+2] != 1 || frame[i+3] != 0xB5 {
			continue
		}
		r := &bitReader{data: frame[i+4:]}
		switch r.bits(4) {
		case 1: // sequence extension
			r.bits(8)
			if p := r.bit(); !r.err {
				s.progressiveSeq = s.progressiveSeq || p == 1
			}
		case 8: // picture coding extension
			r.bits(16) // f_codes
			r.bits(2)  // intra_dc_precision
			structure := r.bits(2)
			tff := r.bit() == 1
			r.bits(6) // frame_pred_frame_dct .. repeat_first_field
			r.bit()   // chroma_420_type
			progressiveFrame := r.bit() == 1
			if r.err || structure == 0 {
				continue
			}
			s.picture(structure, tff, progressiveFrame)
		}
	}
}

func (s *mpeg2Scan) picture(structure uint32, tff, progressiveFrame bool) {
	if structure != 3 { // a field picture: the first of a pair is shown first
		s.interlaced = true
		if !s.firstField {
			s.orders[orderOf(structure == 1)] = true
		}
		s.firstField = !s.firstField
		return
	}
	s.firstField = false
	if !progressiveFrame {
		s.interlaced = true
		s.orders[orderOf(tff)] = true
	}
}

func (s *mpeg2Scan) done() bool {
	return (s.interlaced && len(s.orders) > 1) || (s.progressiveSeq && !s.interlaced)
}

func (s *mpeg2Scan) apply(t *mkv.Track) {
	if t.ScanType != "" {
		return
	}
	switch {
	case s.interlaced:
		t.ScanType = "interlaced"
		if len(s.orders) == 1 && t.FieldOrder == "" {
			for o := range s.orders {
				t.FieldOrder = o
			}
		}
	case s.progressiveSeq:
		t.ScanType, t.FieldOrder = "progressive", "progressive"
	}
}

// InBandScan settles the scan type of an H.264, HEVC or MPEG-2 track over its first
// samples, the way WithInBandColourFallback does for Matroska (see there for what
// counts as proof). Exposed so the mp4 package can drive it off its sample table.
type InBandScan struct{ s scanner }

// NewInBandScan returns the scan for t, nil when t needs none: not H.264, HEVC or MPEG-2,
// a scan type already stated, or an H.264 SPS that codes frames only.
func NewInBandScan(t *mkv.Track) *InBandScan {
	if s := newScanner(t); s != nil {
		return &InBandScan{s}
	}
	return nil
}

// Feed reads one sample, or its first bytes when truncated. It reports whether
// more samples can no longer change the verdict.
func (s *InBandScan) Feed(sample []byte, truncated bool) bool {
	s.s.feed(sample, truncated)
	return s.s.done()
}

// Apply writes the verdict into t: ScanType and FieldOrder on proof, nothing otherwise.
func (s *InBandScan) Apply(t *mkv.Track) { s.s.apply(t) }

func isAVCCodec(codec string) bool {
	switch codec {
	case "h264", "avc", "V_MPEG4/ISO/AVC":
		return true
	}
	return false
}

// needsInBandScan reports whether t's scan type is left for its samples to settle.
func needsInBandScan(t *mkv.Track) bool { return newScanner(t) != nil }

// hevcScan gathers, over the first samples of an HEVC track whose record leaves
// the scan type open (both source flags set, or neither), the proof the stream
// gives: an SPS whose pictures are fields, or picture timing SEI saying a picture
// is interlaced (source_scan_type 0) or a field (pic_struct 1, 2, 9-12) - unless
// that picture says its source is progressive.
type hevcScan struct {
	nalLen         int
	frameFieldInfo bool // pic timing SEI open on pic_struct and source_scan_type
	interlaced     bool
	orders         map[string]bool
}

func newHEVCScan(cp []byte) *hevcScan {
	if len(cp) < 23 || cp[0] != 1 {
		return nil
	}
	s := &hevcScan{nalLen: int(cp[21]&0x03) + 1, orders: map[string]bool{}}
	if bc := hevcColour(cp); bc != nil {
		s.frameFieldInfo = bc.hevcFrameFieldInfo
	}
	return s
}

func (s *hevcScan) feed(frame []byte, truncated bool) {
	defer func() { _ = recover() }() // the walkers bound every length; a frame they misread must not end the read
	s.walk(frame, truncated)
}

// walk is feed without the backstop, for the fuzz target.
func (s *hevcScan) walk(frame []byte, truncated bool) {
	forEachLengthPrefixed(frame, s.nalLen, truncated, func(nal []byte) {
		if len(nal) < 3 {
			return
		}
		switch (nal[0] >> 1) & 0x3f {
		case 33: // SPS in-band
			bc := &bitstreamColour{}
			parseHEVCSPS(unescapeRBSP(nal[2:]), bc)
			s.frameFieldInfo = bc.hevcFrameFieldInfo
			s.interlaced = s.interlaced || bc.hevcFieldSeq
		case 39: // prefix SEI
			if s.frameFieldInfo {
				forEachSEIMessage(unescapeRBSP(nal[2:]), func(pt int, payload []byte) {
					if pt == 1 { // pic_timing
						s.picTiming(payload)
					}
				})
			}
		}
	})
}

// picTiming reads pic_struct and source_scan_type, which open an HEVC picture
// timing SEI when frame_field_info_present_flag is set.
func (s *hevcScan) picTiming(payload []byte) {
	r := &bitReader{data: payload}
	ps, sst := r.bits(4), r.bits(2)
	if r.err || ps > 12 || sst == 1 { // sst 1: this picture's source is progressive
		return
	}
	field := ps == 1 || ps == 2 || ps >= 9
	if sst != 0 && !field {
		return
	}
	s.interlaced = true
	switch ps {
	case 3, 5, 10, 11: // top shown first: a top-bottom frame, a bottom field after its top, a top field before its bottom
		s.orders["tt"] = true
	case 4, 6, 9, 12:
		s.orders["bb"] = true
	}
}

func (s *hevcScan) done() bool { return s.interlaced && len(s.orders) > 1 }

func (s *hevcScan) apply(t *mkv.Track) {
	if t.ScanType != "" || !s.interlaced {
		return
	}
	t.ScanType = "interlaced"
	if len(s.orders) == 1 && t.FieldOrder == "" {
		for o := range s.orders {
			t.FieldOrder = o
		}
	}
}
