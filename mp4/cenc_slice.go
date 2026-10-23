package mp4

// cenc_slice.go - the H.264/HEVC subsample layout of ISO/IEC 23001-7 (3rd
// edition) and CMAF: a NAL unit that is not a coded slice stays clear, a slice
// keeps its NAL header and slice header clear, and under 'cenc' its protected
// bytes are a whole number of 16-byte blocks (the remainder joins the clear
// bytes in front). Reading a slice header takes its parameter sets: those of
// the track's CodecPrivate, then those the segment carries in band, in decode
// order. The splitter is built per segment, so a segment reads the same
// parameter sets whether it is packaged alone (on demand) or in a full pass.

import (
	"errors"
	"fmt"
	"math/bits"
)

// h26xSplitter computes a H.264/HEVC sample's subsamples.
type h26xSplitter struct {
	hevc    bool
	lenSize int  // NAL length field size (avcC/hvcC lengthSizeMinusOne + 1)
	align   bool // 'cenc': protected bytes in whole AES blocks
	avcSPS  map[uint32]*avcSPS
	avcPPS  map[uint32]*avcPPS
	hevcSPS map[uint32]*hevcSPS
	hevcPPS map[uint32]*hevcPPS
}

func newH26xSplitter(codec string, private []byte, scheme string) (*h26xSplitter, error) {
	s := &h26xSplitter{hevc: codec == "hevc", lenSize: 4, align: scheme == "cenc"}
	if s.hevc {
		s.hevcSPS, s.hevcPPS = map[uint32]*hevcSPS{}, map[uint32]*hevcPPS{}
	} else {
		s.avcSPS, s.avcPPS = map[uint32]*avcSPS{}, map[uint32]*avcPPS{}
	}
	sets, lenSize, ok := configParameterSets(s.hevc, private)
	if ok {
		if lenSize == 3 {
			return nil, errf("cenc: the track's CodecPrivate declares 3-byte NAL lengths, which no H.264/HEVC stream uses")
		}
		s.lenSize = lenSize
	}
	for _, nal := range sets {
		s.learn(nal)
	}
	return s, nil
}

// configParameterSets is an avcC/hvcC record's parameter sets and NAL length size; ok is false for anything else.
func configParameterSets(hevc bool, private []byte) (sets [][]byte, lenSize int, ok bool) {
	next := func(p []byte) ([]byte, []byte, bool) {
		if len(p) < 2 {
			return nil, nil, false
		}
		n := int(p[0])<<8 | int(p[1])
		if len(p) < 2+n {
			return nil, nil, false
		}
		return p[2 : 2+n], p[2+n:], true
	}
	if hevc {
		if len(private) < 23 {
			return nil, 0, false
		}
		lenSize = int(private[21]&3) + 1
		p := private[23:]
		for a := 0; a < int(private[22]); a++ {
			if len(p) < 3 {
				break
			}
			count := int(p[1])<<8 | int(p[2])
			p = p[3:]
			for i := 0; i < count; i++ {
				var nal []byte
				if nal, p, ok = next(p); !ok {
					return sets, lenSize, true
				}
				sets = append(sets, nal)
			}
		}
		return sets, lenSize, true
	}
	if len(private) < 7 || private[0] != 1 {
		return nil, 0, false
	}
	lenSize = int(private[4]&3) + 1
	p := private[5:]
	for _, mask := range []byte{0x1f, 0xff} {
		if len(p) < 1 {
			break
		}
		count := int(p[0] & mask)
		p = p[1:]
		for i := 0; i < count; i++ {
			var nal []byte
			if nal, p, ok = next(p); !ok {
				return sets, lenSize, true
			}
			sets = append(sets, nal)
		}
	}
	return sets, lenSize, true
}

func (s *h26xSplitter) split(sample []byte) ([]cencSubsample, error) {
	b := subsampleBuilder{align: s.align}
	for i := 0; i < len(sample); {
		if len(sample)-i < s.lenSize {
			return nil, errf("cenc: truncated NAL length prefix in sample")
		}
		var n uint64
		for _, c := range sample[i : i+s.lenSize] {
			n = n<<8 | uint64(c)
		}
		if n > uint64(len(sample)-i-s.lenSize) {
			return nil, errf("cenc: NAL length %d exceeds sample bounds", n)
		}
		nal := sample[i+s.lenSize : i+s.lenSize+int(n)]
		clear, err := s.clearLen(nal)
		if err != nil {
			return nil, err
		}
		b.add(s.lenSize+clear, len(nal)-clear)
		i += s.lenSize + len(nal)
	}
	return b.done(), nil
}

// clearLen is how many leading bytes of nal stay clear: all but a coded slice's data.
func (s *h26xSplitter) clearLen(nal []byte) (int, error) {
	if s.hevc {
		if len(nal) < 2 {
			return len(nal), nil
		}
		typ, layer := nal[0]>>1&0x3f, (nal[0]&1)<<5|nal[1]>>3
		if typ >= 32 {
			s.learn(nal)
			return len(nal), nil
		}
		if layer != 0 {
			return 0, errf("cenc: a HEVC slice of layer %d: only single-layer HEVC is packaged with CENC", layer)
		}
		if typ >= 22 {
			return 0, errf("cenc: a HEVC slice of reserved NAL unit type %d", typ)
		}
		hdr, err := s.hevcSliceHeaderLen(nal[2:], typ)
		if err != nil {
			return 0, errf("cenc: hevc: %v; this video cannot be encrypted with CENC", err)
		}
		return 2 + hdr, nil
	}
	if len(nal) < 1 {
		return 0, nil
	}
	switch typ := nal[0] & 0x1f; typ {
	case 1, 5:
		hdr, err := s.avcSliceHeaderLen(nal[1:], typ, nal[0]>>5&3)
		if err != nil {
			return 0, errf("cenc: h264: %v; this video cannot be encrypted with CENC", err)
		}
		return 1 + hdr, nil
	default:
		s.learn(nal)
		return len(nal), nil
	}
}

// learn records a parameter set; one it cannot read is refused only when a slice refers to it.
func (s *h26xSplitter) learn(nal []byte) {
	if s.hevc {
		if len(nal) < 2 || nal[0]&1 != 0 || nal[1]>>3 != 0 {
			return
		}
		r := newNALReader(nal[2:])
		switch nal[0] >> 1 & 0x3f {
		case 33:
			if id, sps, ok := parseHEVCSPS(r); ok {
				s.hevcSPS[id] = sps
			}
		case 34:
			if id, pps, ok := parseHEVCPPS(r); ok {
				s.hevcPPS[id] = pps
			}
		}
		return
	}
	if len(nal) < 1 {
		return
	}
	r := newNALReader(nal[1:])
	switch nal[0] & 0x1f {
	case 7:
		if id, sps, ok := parseAVCSPS(r); ok {
			s.avcSPS[id] = sps
		}
	case 8:
		if id, pps, ok := parseAVCPPS(r); ok {
			s.avcPPS[id] = pps
		}
	}
}

// subsampleBuilder merges clear-only runs into the next run's clear bytes, splitting clear runs past 16 bits.
type subsampleBuilder struct {
	align bool
	acc   int
	subs  []cencSubsample
}

func (b *subsampleBuilder) add(clear, protected int) {
	if b.align {
		clear += protected % 16
		protected -= protected % 16
	}
	b.acc += clear
	if protected > 0 {
		b.push(protected)
	}
}

func (b *subsampleBuilder) push(protected int) {
	for ; b.acc > 0xFFFF; b.acc -= 0xFFFF {
		b.subs = append(b.subs, cencSubsample{clear: 0xFFFF})
	}
	b.subs = append(b.subs, cencSubsample{clear: uint16(b.acc), protected: uint32(protected)})
	b.acc = 0
}

func (b *subsampleBuilder) done() []cencSubsample {
	if b.acc > 0 || len(b.subs) == 0 {
		b.push(0)
	}
	return b.subs
}

// nalReader reads an escaped NAL payload bit by bit past its emulation prevention bytes; pos counts escaped bytes.
type nalReader struct {
	data  []byte
	pos   int
	zeros int
	cur   byte
	left  int
	err   bool
}

func newNALReader(data []byte) *nalReader { return &nalReader{data: data} }

func (r *nalReader) bit() uint32 {
	if r.left == 0 {
		if r.zeros >= 2 && r.pos < len(r.data) && r.data[r.pos] == 3 {
			r.pos++
			r.zeros = 0
		}
		if r.pos >= len(r.data) {
			r.err = true
			return 0
		}
		r.cur = r.data[r.pos]
		r.pos++
		r.left = 8
		if r.cur == 0 {
			r.zeros++
		} else {
			r.zeros = 0
		}
	}
	r.left--
	return uint32(r.cur>>r.left) & 1
}

func (r *nalReader) u(n int) uint32 {
	var v uint32
	for ; n > 0; n-- {
		v = v<<1 | r.bit()
	}
	return v
}

func (r *nalReader) flag() bool { return r.bit() == 1 }

func (r *nalReader) skip(n int) {
	for ; n > 0 && !r.err; n-- {
		r.bit()
	}
}

// ue reads an Exp-Golomb code, at most 32 bits of value.
func (r *nalReader) ue() uint32 {
	zeros := 0
	for r.bit() == 0 {
		if zeros++; zeros > 31 || r.err {
			r.err = true
			return 0
		}
	}
	return 1<<zeros - 1 + r.u(zeros)
}

func (r *nalReader) se() int32 {
	v := r.ue()
	if v&1 == 1 {
		return int32(v/2 + 1)
	}
	return -int32(v / 2)
}

// headerLen is the escaped bytes read so far, the partly read one included.
func (r *nalReader) headerLen() (int, error) {
	if r.err {
		return 0, errors.New("the slice header cannot be read: it runs past its NAL unit or holds a value out of range")
	}
	return r.pos, nil
}

// ceilLog2 is Ceil(Log2(n)), the width of a field indexing n values.
func ceilLog2(n uint32) int {
	if n <= 1 {
		return 0
	}
	return bits.Len32(n - 1)
}

// --- H.264 (ITU-T H.264 7.3.2.1.1, 7.3.2.2, 7.3.3) -----------------------

type avcSPS struct {
	chromaArrayType         uint32
	separateColourPlane     bool
	log2MaxFrameNum         int
	pocType                 uint32
	log2MaxPocLsb           int
	deltaPicOrderAlwaysZero bool
	frameMbsOnly            bool
}

type avcPPS struct {
	spsID                    uint32
	cabac                    bool
	bottomFieldPicOrder      bool
	sliceGroups              bool
	numRefIdxL0, numRefIdxL1 uint32 // default num_ref_idx_lX_active_minus1
	weightedPred             bool
	weightedBipredIdc        uint32
	deblockingFilterControl  bool
	redundantPicCntPresent   bool
}

func parseAVCSPS(r *nalReader) (uint32, *avcSPS, bool) {
	profile := r.u(8)
	r.skip(16) // constraint flags, level_idc
	id := r.ue()
	sps := &avcSPS{chromaArrayType: 1}
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma := r.ue()
		sps.chromaArrayType = chroma
		if chroma == 3 {
			sps.separateColourPlane = r.flag()
			if sps.separateColourPlane {
				sps.chromaArrayType = 0
			}
		}
		r.ue() // bit_depth_luma_minus8
		r.ue() // bit_depth_chroma_minus8
		r.skip(1)
		if r.flag() { // seq_scaling_matrix_present_flag
			lists := 8
			if chroma == 3 {
				lists = 12
			}
			for i := 0; i < lists; i++ {
				if r.flag() {
					size := 16
					if i >= 6 {
						size = 64
					}
					skipScalingList(r, size)
				}
			}
		}
	}
	sps.log2MaxFrameNum = int(r.ue()) + 4
	sps.pocType = r.ue()
	switch sps.pocType {
	case 0:
		sps.log2MaxPocLsb = int(r.ue()) + 4
	case 1:
		sps.deltaPicOrderAlwaysZero = r.flag()
		r.se()
		r.se()
		n := r.ue()
		if n > 255 {
			return 0, nil, false
		}
		for i := uint32(0); i < n; i++ {
			r.se()
		}
	}
	r.ue()    // max_num_ref_frames
	r.skip(1) // gaps_in_frame_num_value_allowed_flag
	r.ue()    // pic_width_in_mbs_minus1
	r.ue()    // pic_height_in_map_units_minus1
	sps.frameMbsOnly = r.flag()
	if r.err || id > 31 || sps.log2MaxFrameNum > 16 || sps.pocType > 2 || sps.log2MaxPocLsb > 16 || sps.chromaArrayType > 3 {
		return 0, nil, false
	}
	return id, sps, true
}

func skipScalingList(r *nalReader, size int) {
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

func parseAVCPPS(r *nalReader) (uint32, *avcPPS, bool) {
	id := r.ue()
	pps := &avcPPS{spsID: r.ue()}
	pps.cabac = r.flag()
	pps.bottomFieldPicOrder = r.flag()
	if r.ue() > 0 { // num_slice_groups_minus1: the rest is not read, a slice refuses it
		pps.sliceGroups = true
		return id, pps, !r.err && id <= 255
	}
	pps.numRefIdxL0 = r.ue()
	pps.numRefIdxL1 = r.ue()
	pps.weightedPred = r.flag()
	pps.weightedBipredIdc = r.u(2)
	r.se() // pic_init_qp_minus26
	r.se() // pic_init_qs_minus26
	r.se() // chroma_qp_index_offset
	pps.deblockingFilterControl = r.flag()
	r.skip(1) // constrained_intra_pred_flag
	pps.redundantPicCntPresent = r.flag()
	if r.err || id > 255 || pps.spsID > 31 || pps.numRefIdxL0 > 31 || pps.numRefIdxL1 > 31 {
		return 0, nil, false
	}
	return id, pps, true
}

// avcSliceHeaderLen is the escaped size of a H.264 slice header, payload starting after the NAL header.
func (s *h26xSplitter) avcSliceHeaderLen(payload []byte, nalType, refIdc byte) (int, error) {
	r := newNALReader(payload)
	r.ue() // first_mb_in_slice
	sliceType := r.ue() % 5
	ppsID := r.ue()
	pps := s.avcPPS[ppsID]
	if r.err {
		return r.headerLen()
	}
	if pps == nil {
		return 0, fmt.Errorf("a slice refers to picture parameter set %d, which the stream does not carry or mkvgo cannot read", ppsID)
	}
	if pps.sliceGroups {
		return 0, errors.New("slice groups (FMO) are not supported")
	}
	sps := s.avcSPS[pps.spsID]
	if sps == nil {
		return 0, fmt.Errorf("a slice refers to sequence parameter set %d, which the stream does not carry or mkvgo cannot read", pps.spsID)
	}
	const (
		sliceP, sliceB, sliceI, sliceSP, sliceSI = 0, 1, 2, 3, 4
	)
	if sps.separateColourPlane {
		r.skip(2)
	}
	r.skip(sps.log2MaxFrameNum)
	field := false
	if !sps.frameMbsOnly {
		if field = r.flag(); field {
			r.skip(1)
		}
	}
	if nalType == 5 {
		r.ue() // idr_pic_id
	}
	switch {
	case sps.pocType == 0:
		r.skip(sps.log2MaxPocLsb)
		if pps.bottomFieldPicOrder && !field {
			r.se()
		}
	case sps.pocType == 1 && !sps.deltaPicOrderAlwaysZero:
		r.se()
		if pps.bottomFieldPicOrder && !field {
			r.se()
		}
	}
	if pps.redundantPicCntPresent {
		r.ue()
	}
	if sliceType == sliceB {
		r.skip(1) // direct_spatial_mv_pred_flag
	}
	l0, l1 := pps.numRefIdxL0, pps.numRefIdxL1
	if sliceType == sliceP || sliceType == sliceSP || sliceType == sliceB {
		if r.flag() { // num_ref_idx_active_override_flag
			l0 = r.ue()
			if sliceType == sliceB {
				l1 = r.ue()
			}
		}
	}
	if l0 > 31 || l1 > 31 {
		return 0, fmt.Errorf("a slice with %d reference indexes", max(l0, l1)+1)
	}
	if sliceType != sliceI && sliceType != sliceSI {
		lists := 1
		if sliceType == sliceB {
			lists = 2
		}
		for ; lists > 0; lists-- {
			if !r.flag() { // ref_pic_list_modification_flag_lX
				continue
			}
			for n := 0; ; n++ {
				idc := r.ue()
				if idc == 3 || r.err {
					break
				}
				if idc > 2 || n > 64 {
					return 0, errors.New("a reference list modification it cannot read")
				}
				r.ue()
			}
		}
	}
	if (pps.weightedPred && (sliceType == sliceP || sliceType == sliceSP)) || (pps.weightedBipredIdc == 1 && sliceType == sliceB) {
		r.ue() // luma_log2_weight_denom
		chroma := sps.chromaArrayType != 0
		if chroma {
			r.ue()
		}
		counts := []uint32{l0}
		if sliceType == sliceB {
			counts = append(counts, l1)
		}
		for _, n := range counts {
			for j := uint32(0); j <= n && !r.err; j++ {
				if r.flag() {
					r.se()
					r.se()
				}
				if chroma && r.flag() {
					r.se()
					r.se()
					r.se()
					r.se()
				}
			}
		}
	}
	if refIdc != 0 {
		if nalType == 5 {
			r.skip(2) // no_output_of_prior_pics_flag, long_term_reference_flag
		} else if r.flag() { // adaptive_ref_pic_marking_mode_flag
			for n := 0; ; n++ {
				op := r.ue()
				if op == 0 || r.err {
					break
				}
				if op > 6 || n > 64 {
					return 0, errors.New("a reference picture marking it cannot read")
				}
				if op == 1 || op == 3 {
					r.ue()
				}
				if op == 2 {
					r.ue()
				}
				if op == 3 || op == 6 {
					r.ue()
				}
				if op == 4 {
					r.ue()
				}
			}
		}
	}
	if pps.cabac && sliceType != sliceI && sliceType != sliceSI {
		r.ue() // cabac_init_idc
	}
	r.se() // slice_qp_delta
	if sliceType == sliceSP || sliceType == sliceSI {
		if sliceType == sliceSP {
			r.skip(1)
		}
		r.se()
	}
	if pps.deblockingFilterControl {
		if r.ue() != 1 {
			r.se()
			r.se()
		}
	}
	return r.headerLen()
}

// --- HEVC (ITU-T H.265 7.3.2.2, 7.3.2.3, 7.3.6.1, 7.3.7) -------------------

// stRPS is a short-term reference picture set: delta POCs before (s0) and after (s1), and which are used.
type stRPS struct {
	s0, s1       []int32
	used0, used1 []bool
}

func (r stRPS) numDeltaPocs() int { return len(r.s0) + len(r.s1) }

func countTrue(v []bool) int {
	n := 0
	for _, b := range v {
		if b {
			n++
		}
	}
	return n
}

type hevcSPS struct {
	chromaArrayType     uint32
	separateColourPlane bool
	log2MaxPocLsb       int
	picSizeInCtbs       uint32
	rps                 []stRPS
	longTermRefs        bool
	ltUsed              []bool // used_by_curr_pic_lt_sps_flag
	temporalMVP         bool
	sao                 bool
}

type hevcPPS struct {
	spsID                    uint32
	dependentSliceSegments   bool
	outputFlagPresent        bool
	extraSliceHeaderBits     int
	cabacInitPresent         bool
	numRefIdxL0, numRefIdxL1 uint32
	sliceChromaQPOffsets     bool
	weightedPred             bool
	weightedBipred           bool
	tilesOrEntropySync       bool
	loopFilterAcrossSlices   bool
	deblockingOverride       bool
	deblockingDisabled       bool
	listsModification        bool
	headerExtension          bool
	chromaQPOffsetList       bool
}

func parseHEVCSPS(r *nalReader) (uint32, *hevcSPS, bool) {
	r.skip(4) // sps_video_parameter_set_id
	maxSubLayersMinus1 := int(r.u(3))
	r.skip(1)
	if !skipProfileTierLevel(r, maxSubLayersMinus1) {
		return 0, nil, false
	}
	id := r.ue()
	sps := &hevcSPS{}
	chroma := r.ue()
	sps.chromaArrayType = chroma
	if chroma == 3 {
		sps.separateColourPlane = r.flag()
		if sps.separateColourPlane {
			sps.chromaArrayType = 0
		}
	}
	width, height := r.ue(), r.ue()
	if r.flag() { // conformance_window_flag
		r.ue()
		r.ue()
		r.ue()
		r.ue()
	}
	r.ue() // bit_depth_luma_minus8
	r.ue() // bit_depth_chroma_minus8
	sps.log2MaxPocLsb = int(r.ue()) + 4
	first := maxSubLayersMinus1
	if r.flag() { // sps_sub_layer_ordering_info_present_flag
		first = 0
	}
	for i := first; i <= maxSubLayersMinus1; i++ {
		r.ue()
		r.ue()
		r.ue()
	}
	log2MinCb := r.ue() + 3
	log2Ctb := log2MinCb + r.ue()
	r.ue() // log2_min_luma_transform_block_size_minus2
	r.ue() // log2_diff_max_min_luma_transform_block_size
	r.ue() // max_transform_hierarchy_depth_inter
	r.ue() // max_transform_hierarchy_depth_intra
	// scaling_list_enabled_flag, then sps_scaling_list_data_present_flag
	if scalingList := r.flag(); scalingList && r.flag() {
		skipScalingListData(r)
	}
	r.skip(1) // amp_enabled_flag
	sps.sao = r.flag()
	if r.flag() { // pcm_enabled_flag
		r.skip(8)
		r.ue()
		r.ue()
		r.skip(1)
	}
	if r.err || id > 15 || sps.log2MaxPocLsb > 16 || chroma > 3 || log2Ctb < 4 || log2Ctb > 6 || width == 0 || height == 0 || width > 1<<16 || height > 1<<16 {
		return 0, nil, false
	}
	ctb := uint32(1) << log2Ctb
	sps.picSizeInCtbs = ((width + ctb - 1) / ctb) * ((height + ctb - 1) / ctb)
	n := r.ue() // num_short_term_ref_pic_sets
	if n > 64 {
		return 0, nil, false
	}
	for i := 0; i < int(n); i++ {
		rps, ok := parseSTRPS(r, sps.rps, i, int(n))
		if !ok {
			return 0, nil, false
		}
		sps.rps = append(sps.rps, rps)
	}
	if sps.longTermRefs = r.flag(); sps.longTermRefs {
		lt := r.ue()
		if lt > 32 {
			return 0, nil, false
		}
		for i := uint32(0); i < lt; i++ {
			r.skip(sps.log2MaxPocLsb)
			sps.ltUsed = append(sps.ltUsed, r.flag())
		}
	}
	sps.temporalMVP = r.flag()
	if r.err {
		return 0, nil, false
	}
	return id, sps, true
}

// skipProfileTierLevel skips profile_tier_level(1, n), refusing the screen content profile (other slice headers).
func skipProfileTierLevel(r *nalReader, maxSubLayersMinus1 int) bool {
	r.skip(3)
	profile := r.u(5)
	compat := r.u(32)
	r.skip(4 + 43 + 1 + 8)
	if profile == 9 || compat&(1<<(31-9)) != 0 {
		return false
	}
	var profilePresent, levelPresent [8]bool
	for i := 0; i < maxSubLayersMinus1; i++ {
		profilePresent[i], levelPresent[i] = r.flag(), r.flag()
	}
	if maxSubLayersMinus1 > 0 {
		r.skip(2 * (8 - maxSubLayersMinus1))
	}
	for i := 0; i < maxSubLayersMinus1; i++ {
		if profilePresent[i] {
			r.skip(88)
		}
		if levelPresent[i] {
			r.skip(8)
		}
	}
	return !r.err
}

func skipScalingListData(r *nalReader) {
	for size := 0; size < 4; size++ {
		step := 1
		if size == 3 {
			step = 3
		}
		for m := 0; m < 6; m += step {
			if !r.flag() { // scaling_list_pred_mode_flag
				r.ue()
				continue
			}
			coefs := min(64, 1<<(4+(size<<1)))
			if size > 1 {
				r.se()
			}
			for k := 0; k < coefs && !r.err; k++ {
				r.se()
			}
		}
	}
}

// parseSTRPS reads st_ref_pic_set(idx) against the sets before it; idx == num is a slice header's own set.
func parseSTRPS(r *nalReader, sets []stRPS, idx, num int) (stRPS, bool) {
	var out stRPS
	if idx != 0 && r.flag() { // inter_ref_pic_set_prediction_flag
		delta := 1
		if idx == num {
			delta = int(r.ue()) + 1
		}
		if delta > idx {
			return out, false
		}
		ref := sets[idx-delta]
		sign := r.bit()
		abs := int32(r.ue()) + 1
		if r.err || abs > 1<<15 {
			return out, false
		}
		deltaRps := abs
		if sign == 1 {
			deltaRps = -abs
		}
		n := ref.numDeltaPocs()
		used := make([]bool, n+1)
		use := make([]bool, n+1)
		for j := 0; j <= n; j++ {
			used[j] = r.flag()
			use[j] = true
			if !used[j] {
				use[j] = r.flag()
			}
		}
		neg := len(ref.s0)
		for j := len(ref.s1) - 1; j >= 0; j-- {
			if d := ref.s1[j] + deltaRps; d < 0 && use[neg+j] {
				out.s0, out.used0 = append(out.s0, d), append(out.used0, used[neg+j])
			}
		}
		if deltaRps < 0 && use[n] {
			out.s0, out.used0 = append(out.s0, deltaRps), append(out.used0, used[n])
		}
		for j := 0; j < neg; j++ {
			if d := ref.s0[j] + deltaRps; d < 0 && use[j] {
				out.s0, out.used0 = append(out.s0, d), append(out.used0, used[j])
			}
		}
		for j := neg - 1; j >= 0; j-- {
			if d := ref.s0[j] + deltaRps; d > 0 && use[j] {
				out.s1, out.used1 = append(out.s1, d), append(out.used1, used[j])
			}
		}
		if deltaRps > 0 && use[n] {
			out.s1, out.used1 = append(out.s1, deltaRps), append(out.used1, used[n])
		}
		for j := 0; j < len(ref.s1); j++ {
			if d := ref.s1[j] + deltaRps; d > 0 && use[neg+j] {
				out.s1, out.used1 = append(out.s1, d), append(out.used1, used[neg+j])
			}
		}
		return out, !r.err && out.numDeltaPocs() <= 32
	}
	neg, pos := r.ue(), r.ue()
	if neg > 16 || pos > 16 {
		return out, false
	}
	var poc int32
	for j := uint32(0); j < neg; j++ {
		poc -= int32(r.ue()) + 1
		out.s0, out.used0 = append(out.s0, poc), append(out.used0, r.flag())
	}
	poc = 0
	for j := uint32(0); j < pos; j++ {
		poc += int32(r.ue()) + 1
		out.s1, out.used1 = append(out.s1, poc), append(out.used1, r.flag())
	}
	return out, !r.err
}

func parseHEVCPPS(r *nalReader) (uint32, *hevcPPS, bool) {
	id := r.ue()
	pps := &hevcPPS{spsID: r.ue()}
	pps.dependentSliceSegments = r.flag()
	pps.outputFlagPresent = r.flag()
	pps.extraSliceHeaderBits = int(r.u(3))
	r.skip(1) // sign_data_hiding_enabled_flag
	pps.cabacInitPresent = r.flag()
	pps.numRefIdxL0 = r.ue()
	pps.numRefIdxL1 = r.ue()
	r.se()    // init_qp_minus26
	r.skip(1) // constrained_intra_pred_flag
	transformSkip := r.flag()
	if r.flag() { // cu_qp_delta_enabled_flag
		r.ue()
	}
	r.se() // pps_cb_qp_offset
	r.se() // pps_cr_qp_offset
	pps.sliceChromaQPOffsets = r.flag()
	pps.weightedPred = r.flag()
	pps.weightedBipred = r.flag()
	r.skip(1) // transquant_bypass_enabled_flag
	tiles := r.flag()
	sync := r.flag()
	pps.tilesOrEntropySync = tiles || sync
	if tiles {
		cols, rows := r.ue(), r.ue()
		if cols > 63 || rows > 63 {
			return 0, nil, false
		}
		if !r.flag() { // uniform_spacing_flag
			for i := uint32(0); i < cols+rows; i++ {
				r.ue()
			}
		}
		r.skip(1) // loop_filter_across_tiles_enabled_flag
	}
	pps.loopFilterAcrossSlices = r.flag()
	if r.flag() { // deblocking_filter_control_present_flag
		pps.deblockingOverride = r.flag()
		if pps.deblockingDisabled = r.flag(); !pps.deblockingDisabled {
			r.se()
			r.se()
		}
	}
	if r.flag() { // pps_scaling_list_data_present_flag
		skipScalingListData(r)
	}
	pps.listsModification = r.flag()
	r.ue() // log2_parallel_merge_level_minus2
	pps.headerExtension = r.flag()
	if r.flag() { // pps_extension_present_flag
		rangeExt, _, ext3D, scc := r.flag(), r.flag(), r.flag(), r.flag()
		r.skip(4)
		if ext3D || scc {
			return 0, nil, false
		}
		if rangeExt {
			if transformSkip {
				r.ue()
			}
			r.skip(1) // cross_component_prediction_enabled_flag
			pps.chromaQPOffsetList = r.flag()
		}
	}
	if r.err || id > 63 || pps.spsID > 15 || pps.numRefIdxL0 > 14 || pps.numRefIdxL1 > 14 {
		return 0, nil, false
	}
	return id, pps, true
}

// hevcSliceHeaderLen is the escaped size of a HEVC slice segment header, payload starting after the NAL header.
func (s *h26xSplitter) hevcSliceHeaderLen(payload []byte, nalType byte) (int, error) {
	r := newNALReader(payload)
	firstInPic := r.flag()
	if nalType >= 16 && nalType <= 23 {
		r.skip(1) // no_output_of_prior_pics_flag
	}
	ppsID := r.ue()
	if r.err {
		return r.headerLen()
	}
	pps := s.hevcPPS[ppsID]
	if pps == nil {
		return 0, fmt.Errorf("a slice refers to picture parameter set %d, which the stream does not carry or mkvgo cannot read", ppsID)
	}
	sps := s.hevcSPS[pps.spsID]
	if sps == nil {
		return 0, fmt.Errorf("a slice refers to sequence parameter set %d, which the stream does not carry or mkvgo cannot read", pps.spsID)
	}
	dependent := false
	if !firstInPic {
		if pps.dependentSliceSegments {
			dependent = r.flag()
		}
		r.skip(ceilLog2(sps.picSizeInCtbs))
	}
	if !dependent {
		const b, p = 0, 1
		r.skip(pps.extraSliceHeaderBits)
		sliceType := r.ue()
		if pps.outputFlagPresent {
			r.skip(1)
		}
		if sps.separateColourPlane {
			r.skip(2)
		}
		var cur stRPS
		ltUsed := 0
		temporalMVP := false
		if nalType != 19 && nalType != 20 { // not IDR
			r.skip(sps.log2MaxPocLsb)
			if !r.flag() { // short_term_ref_pic_set_sps_flag
				rps, ok := parseSTRPS(r, sps.rps, len(sps.rps), len(sps.rps))
				if !ok {
					return 0, errors.New("a short-term reference picture set it cannot read")
				}
				cur = rps
			} else {
				idx := uint32(0)
				if len(sps.rps) > 1 {
					idx = r.u(ceilLog2(uint32(len(sps.rps))))
				}
				if int(idx) >= len(sps.rps) {
					return 0, fmt.Errorf("a slice refers to short-term reference picture set %d of %d", idx, len(sps.rps))
				}
				cur = sps.rps[idx]
			}
			if sps.longTermRefs {
				var fromSPS uint32
				if len(sps.ltUsed) > 0 {
					fromSPS = r.ue()
				}
				pics := r.ue()
				if fromSPS > uint32(len(sps.ltUsed)) || pics > 32 {
					return 0, fmt.Errorf("a slice with %d long-term reference pictures", fromSPS+pics)
				}
				for i := uint32(0); i < fromSPS+pics && !r.err; i++ {
					if i < fromSPS {
						idx := uint32(0)
						if len(sps.ltUsed) > 1 {
							idx = r.u(ceilLog2(uint32(len(sps.ltUsed))))
						}
						if int(idx) >= len(sps.ltUsed) {
							return 0, fmt.Errorf("a slice refers to long-term reference picture %d of %d", idx, len(sps.ltUsed))
						}
						if sps.ltUsed[idx] {
							ltUsed++
						}
					} else {
						r.skip(sps.log2MaxPocLsb)
						if r.flag() {
							ltUsed++
						}
					}
					if r.flag() { // delta_poc_msb_present_flag
						r.ue()
					}
				}
			}
			if sps.temporalMVP {
				temporalMVP = r.flag()
			}
		}
		saoLuma, saoChroma := false, false
		if sps.sao {
			saoLuma = r.flag()
			if sps.chromaArrayType != 0 {
				saoChroma = r.flag()
			}
		}
		if sliceType == p || sliceType == b {
			l0, l1 := pps.numRefIdxL0, pps.numRefIdxL1
			if r.flag() { // num_ref_idx_active_override_flag
				l0 = r.ue()
				if sliceType == b {
					l1 = r.ue()
				}
			}
			if l0 > 14 || l1 > 14 {
				return 0, fmt.Errorf("a slice with %d reference indexes", max(l0, l1)+1)
			}
			total := ltUsed + countTrue(cur.used0) + countTrue(cur.used1)
			if pps.listsModification && total > 1 {
				w := ceilLog2(uint32(total))
				if r.flag() {
					r.skip(w * int(l0+1))
				}
				if sliceType == b && r.flag() {
					r.skip(w * int(l1+1))
				}
			}
			if sliceType == b {
				r.skip(1) // mvd_l1_zero_flag
			}
			if pps.cabacInitPresent {
				r.skip(1)
			}
			if temporalMVP {
				fromL0 := true
				if sliceType == b {
					fromL0 = r.flag()
				}
				if (fromL0 && l0 > 0) || (!fromL0 && l1 > 0) {
					r.ue() // collocated_ref_idx
				}
			}
			if (pps.weightedPred && sliceType == p) || (pps.weightedBipred && sliceType == b) {
				r.ue() // luma_log2_weight_denom
				chroma := sps.chromaArrayType != 0
				if chroma {
					r.se()
				}
				counts := []uint32{l0}
				if sliceType == b {
					counts = append(counts, l1)
				}
				for _, n := range counts {
					var luma, chromaW [16]bool
					for j := uint32(0); j <= n; j++ {
						luma[j] = r.flag()
					}
					if chroma {
						for j := uint32(0); j <= n; j++ {
							chromaW[j] = r.flag()
						}
					}
					for j := uint32(0); j <= n && !r.err; j++ {
						if luma[j] {
							r.se()
							r.se()
						}
						if chromaW[j] {
							r.se()
							r.se()
							r.se()
							r.se()
						}
					}
				}
			}
			r.ue() // five_minus_max_num_merge_cand
		} else if sliceType != 2 {
			return 0, fmt.Errorf("slice type %d", sliceType)
		}
		r.se() // slice_qp_delta
		if pps.sliceChromaQPOffsets {
			r.se()
			r.se()
		}
		if pps.chromaQPOffsetList {
			r.skip(1)
		}
		deblockingDisabled := pps.deblockingDisabled
		if pps.deblockingOverride && r.flag() {
			if deblockingDisabled = r.flag(); !deblockingDisabled {
				r.se()
				r.se()
			}
		}
		if pps.loopFilterAcrossSlices && (saoLuma || saoChroma || !deblockingDisabled) {
			r.skip(1)
		}
	}
	if pps.tilesOrEntropySync {
		n := r.ue()
		if n > 0 {
			w := int(r.ue()) + 1
			if w > 32 || n > 1<<16 {
				return 0, fmt.Errorf("%d entry points of %d bits", n, w)
			}
			r.skip(w * int(n))
		}
	}
	if pps.headerExtension {
		r.skip(8 * int(min(r.ue(), 256)))
	}
	if !r.flag() { // byte_alignment(): alignment_bit_equal_to_one
		return 0, errors.New("the slice header does not end where its syntax says")
	}
	for r.left > 0 {
		if r.bit() != 0 {
			return 0, errors.New("the slice header does not end where its syntax says")
		}
	}
	return r.headerLen()
}
