// Package dovi rewrites the Dolby Vision layers of an HEVC stream without touching its base layer: the reference
// processing unit (RPU) of a dual-layer profile 7 picture becomes a single-layer profile 8.1 one, or leaves with the
// enhancement layer.
package dovi

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// EL is what a profile 7 enhancement layer carries, read from the RPU's residual quantizer.
type EL int

// The enhancement layer kinds.
const (
	ELNone EL = iota // no enhancement layer: the RPU alone
	ELMinimal
	ELFull
)

func (e EL) String() string { return [...]string{"", "MEL", "FEL"}[e] }

// RPU is what the header of one RPU says.
type RPU struct {
	Profile int // 4, 5, 7 or 8; 0 unknown
	EL      EL
}

const (
	rpuPrefix = 0x19
	rpuFinal  = 0x80
	rpuTail   = 5 // the CRC-32 and the final byte

	matrixBits = 9*16 + 3*32 + 9*16 // the YCbCr to RGB matrix, its offsets and the RGB to LMS matrix
)

// rgbToLMS and yccToRGB are the profile 8.1 colour matrices and offsets: BT.2020 non-constant luminance, narrow range.
var (
	yccToRGB       = [9]int16{9574, 0, 13802, 9574, -1540, -5348, 9574, 17610, 0}
	yccToRGBOffset = [3]uint32{16777216, 134217728, 134217728}
	rgbToLMS       = [9]int16{7222, 8771, 390, 2654, 12430, 1300, 0, 422, 15962}
)

type rpuHeader struct {
	format       uint64
	profile      uint64
	level        uint64
	coefType     uint64
	log2Denom    uint64
	fullRange    bool
	blDepth      uint64
	elDepth      uint64
	vdrDepth     uint64
	compressedDM bool
	elFilter     bool
	noResidual   bool
	flagsAt      int // where el_spatial_resampling_filter_flag sits
	dm           bool
	usePrev      bool
}

// coefBits is the width of a coefficient's fractional part.
func (h *rpuHeader) coefBits() int {
	if h.coefType == 0 {
		return int(h.log2Denom)
	}
	return 32
}

func (h *rpuHeader) doviProfile() int {
	switch {
	case h.profile == 0 && h.fullRange:
		return 5
	case h.profile != 1:
		return 0
	case !h.elFilter || h.noResidual:
		return 8
	case h.vdrDepth == 4:
		return 7
	}
	return 4
}

// rpuBody checks an RPU's frame - prefix, CRC-32, final byte - and returns it without its CRC and final byte.
func rpuBody(rpu []byte) ([]byte, error) {
	for len(rpu) > 0 && rpu[len(rpu)-1] == 0 {
		rpu = rpu[:len(rpu)-1]
	}
	switch {
	case len(rpu) < rpuTail+2:
		return nil, errors.New("the RPU is too short")
	case rpu[0] != rpuPrefix:
		return nil, errors.New("the RPU does not open on its prefix byte")
	case rpu[len(rpu)-1] != rpuFinal:
		return nil, errors.New("the RPU does not end on its final byte")
	}
	body := rpu[:len(rpu)-rpuTail]
	if crc32MPEG2(body[1:]) != binary.BigEndian.Uint32(rpu[len(body):]) {
		return nil, errors.New("the RPU checksum does not match its data")
	}
	return body, nil
}

func readHeader(r *bitReader) (*rpuHeader, error) {
	h := &rpuHeader{}
	if r.bits(6) != 2 {
		return nil, errors.New("the RPU type is not 2")
	}
	h.format, h.profile, h.level = r.bits(11), r.bits(4), r.bits(4)
	if !r.flag() || h.format&0x700 != 0 {
		return nil, errors.New("the RPU carries no sequence information")
	}
	r.bits(1) // chroma_resampling_explicit_filter_flag
	if h.coefType = r.bits(2); h.coefType == 0 {
		h.log2Denom = r.ue()
	}
	r.bits(2) // vdr_rpu_normalized_idc
	h.fullRange = r.flag()
	h.blDepth, h.elDepth, h.vdrDepth = r.ue(), r.ue()&0xFF, r.ue()
	r.bits(1) // spatial_resampling_filter_flag
	h.compressedDM = r.bits(3) == 1
	h.flagsAt = r.pos
	h.elFilter, h.noResidual = r.flag(), r.flag()
	h.dm, h.usePrev = r.flag(), r.flag()
	if h.usePrev {
		r.ue()
	}
	switch {
	case r.err != nil:
		return nil, r.err
	case h.coefType > 1:
		return nil, fmt.Errorf("coefficient data type %d is not known", h.coefType)
	case h.level != 0 || h.blDepth != 2 || h.elDepth != 2 || h.vdrDepth > 6 || h.log2Denom > 23:
		return nil, errors.New("the RPU header holds a value out of its range")
	}
	return h, nil
}

// mapping is where the parts of an RPU's reshaping data sit, as bit positions.
type mapping struct {
	start, pivots, nlqPivots, partitions, curves, nlq, end int
	el                                                     EL
}

func readMapping(r *bitReader, h *rpuHeader) (*mapping, error) {
	m := &mapping{start: r.pos}
	r.ue() // vdr_rpu_id
	if space, chroma := r.ue(), r.ue(); space != 0 || chroma != 0 {
		return nil, errors.New("the mapping colour space or chroma format is not 0")
	}
	m.pivots = r.pos
	var pieces [3]int
	for c := range pieces {
		n := r.ue()
		if n > 7 {
			return nil, errors.New("a reshaping curve has more than 8 pieces")
		}
		pieces[c] = int(n) + 1
		r.bits((int(n) + 2) * int(h.blDepth+8))
	}
	m.nlqPivots = r.pos
	if !h.noResidual {
		if r.bits(3) != 0 {
			return nil, errors.New("the residual quantizer method is not known")
		}
		r.bits(2 * int(h.blDepth+8))
	}
	m.partitions = r.pos
	r.ue()
	r.ue()
	m.curves = r.pos
	for _, n := range pieces {
		for ; n > 0; n-- {
			if err := skipPiece(r, h); err != nil {
				return nil, err
			}
		}
	}
	m.nlq = r.pos
	if !h.noResidual {
		m.el = readNLQ(r, h)
	}
	m.end = r.pos
	return m, r.err
}

// skipPiece reads past one piece of a reshaping curve: a polynomial or a multivariate regression.
func skipPiece(r *bitReader, h *rpuHeader) error {
	coef := func() {
		if h.coefType == 0 {
			r.ue()
		}
		r.bits(h.coefBits())
	}
	coefs := 0
	switch r.ue() {
	case 0:
		order := r.ue()
		if order > 1 {
			return errors.New("a polynomial curve is past the second order")
		}
		if order == 0 && r.flag() {
			return errors.New("a linearly interpolated curve is not supported")
		}
		coefs = int(order) + 2
	case 1:
		order := r.bits(2)
		if order > 2 {
			return errors.New("a regression curve is past the third order")
		}
		coefs = 1 + 7*(int(order)+1)
	default:
		return errors.New("a reshaping curve uses an unknown method")
	}
	for ; coefs > 0; coefs-- {
		coef()
	}
	return r.err
}

// readNLQ reads the residual quantizer and tells a minimal enhancement layer, whose residual is null, from a full one.
func readNLQ(r *bitReader, h *rpuHeader) EL {
	el := ELMinimal
	want := func(v, is uint64) {
		if v != is {
			el = ELFull
		}
	}
	coef := func(integer uint64) {
		if h.coefType == 0 {
			want(r.ue(), integer)
		}
		want(r.bits(h.coefBits()), 0)
	}
	for range 3 {
		want(r.bits(int(h.elDepth+8)), 0) // nlq_offset
		coef(1)                           // vdr_in_max
		coef(0)                           // linear_deadzone_slope
		coef(0)                           // linear_deadzone_threshold
	}
	return el
}

// dmLayout is where the parts of an RPU's display management data sit, as bit positions.
type dmLayout struct {
	start, matrices, colourSpace int
	fixed                        bool // the matrices and the signal description are present
	counted, aligned, end        int  // the first block count's end, the byte boundary after it, the data's end
}

// readDM reads past the display management data: its fixed part, then its extension blocks.
func readDM(r *bitReader, h *rpuHeader) (*dmLayout, error) {
	d := &dmLayout{start: r.pos, fixed: !h.compressedDM}
	if r.ue() > 15 { // affected_dm_metadata_id
		return nil, errors.New("the display management id is past 15")
	}
	r.ue() // current_dm_metadata_id
	r.ue() // scene_refresh_flag
	if d.fixed {
		d.matrices = r.pos
		r.bits(matrixBits)
		eotf, p0, p1, p2 := r.bits(16), r.bits(16), r.bits(16), r.bits(32)
		depth := r.bits(5)
		d.colourSpace = r.pos
		r.bits(2 + 2 + 2 + 12 + 12 + 10)
		if r.err == nil && (depth < 8 || depth > 16 || (p0 == 0 && p1 == 0 && p2 == 0 && eotf != 0xFFFF)) {
			return nil, errors.New("the display management signal is out of its range")
		}
	}
	d.counted, d.aligned = skipBlocks(r)
	if r.left() >= 16 {
		skipBlocks(r)
	}
	d.end = r.pos
	return d, r.err
}

// skipBlocks reads past one set of extension blocks, each framed by its length, and returns where its count ends
// and where the blocks, byte-aligned, begin.
func skipBlocks(r *bitReader) (counted, aligned int) {
	n := r.ue()
	counted = r.pos
	if !r.align() && r.err == nil {
		r.err = errors.New("the padding before the extension blocks is not zero")
	}
	aligned = r.pos
	for ; n > 0 && r.err == nil; n-- {
		size := r.ue()
		r.bits(8)
		if size > uint64(r.left()/8) {
			r.err = errShort
			break
		}
		r.pos += int(size) * 8
	}
	return counted, aligned
}

// Read returns what the RPU says of its profile and its enhancement layer.
func Read(rpu []byte) (RPU, error) {
	body, err := rpuBody(rpu)
	if err != nil {
		return RPU{}, err
	}
	r := &bitReader{b: body, pos: 8}
	h, err := readHeader(r)
	if err != nil {
		return RPU{}, err
	}
	out := RPU{Profile: h.doviProfile()}
	if h.usePrev || h.noResidual {
		return out, nil
	}
	m, err := readMapping(r, h)
	if err != nil {
		return RPU{}, err
	}
	out.EL = m.el
	return out, nil
}

// To81 rewrites a profile 7 RPU as profile 8.1: the residual off, its quantizer gone, the display management matrices
// those of profile 8.1 and, when the enhancement layer was a full one, the reshaping curves set to the identity.
func To81(rpu []byte) ([]byte, error) {
	body, err := rpuBody(rpu)
	if err != nil {
		return nil, err
	}
	r := &bitReader{b: body, pos: 8}
	h, err := readHeader(r)
	if err != nil {
		return nil, err
	}
	if p := h.doviProfile(); p != 7 && p != 8 {
		return nil, fmt.Errorf("the RPU is of profile %d, neither 7 nor 8", p)
	}
	w := &bitWriter{b: make([]byte, 0, len(rpu))}
	w.copyBits(body, 0, h.flagsAt)
	w.bits(0b01, 2) // el_spatial_resampling_filter_flag off, disable_residual_flag on
	w.copyBits(body, h.flagsAt+2, r.pos)
	if !h.usePrev {
		m, err := readMapping(r, h)
		if err != nil {
			return nil, err
		}
		if err := writeMapping(w, body, h, m); err != nil {
			return nil, err
		}
	}
	if h.dm {
		d, err := readDM(r, h)
		if err != nil {
			return nil, err
		}
		writeDM(w, body, d)
	}
	if !r.align() {
		return nil, errors.New("the padding closing the RPU is not zero")
	}
	if r.err != nil {
		return nil, r.err
	}
	w.align()
	w.b = append(w.b, body[r.pos>>3:]...)
	w.b = binary.BigEndian.AppendUint32(w.b, crc32MPEG2(w.b[1:]))
	return append(w.b, rpuFinal), nil
}

// writeMapping writes the reshaping data without its residual quantizer, on a single partition.
func writeMapping(w *bitWriter, body []byte, h *rpuHeader, m *mapping) error {
	w.copyBits(body, m.start, m.pivots)
	if m.el != ELFull {
		w.copyBits(body, m.pivots, m.nlqPivots)
		w.ue(0)
		w.ue(0)
		w.copyBits(body, m.curves, m.nlq)
		return nil
	}
	if h.coefType != 0 {
		return errors.New("the identity curve of a full enhancement layer has no floating-point form")
	}
	depth := int(h.blDepth + 8)
	for range 3 {
		w.ue(0) // two pivots, the range's ends
		w.bits(0, depth)
		w.bits(1<<depth-1, depth)
	}
	w.ue(0)
	w.ue(0)
	for range 3 {
		w.ue(0)      // polynomial
		w.ue(0)      // first order
		w.bits(0, 1) // not interpolated
		w.ue(0)      // constant 0
		w.bits(0, h.coefBits())
		w.ue(1) // slope 1, the signed Exp-Golomb code of +1
		w.bits(0, h.coefBits())
	}
	return nil
}

// writeDM writes the display management data with the profile 8.1 matrices and colour space.
func writeDM(w *bitWriter, body []byte, d *dmLayout) {
	at := d.start
	if d.fixed {
		w.copyBits(body, at, d.matrices)
		for _, c := range yccToRGB {
			w.bits(uint64(uint16(c)), 16)
		}
		for _, o := range yccToRGBOffset {
			w.bits(uint64(o), 32)
		}
		for _, c := range rgbToLMS {
			w.bits(uint64(uint16(c)), 16)
		}
		w.copyBits(body, d.matrices+matrixBits, d.colourSpace)
		w.bits(0, 2)
		at = d.colourSpace + 2
	}
	w.copyBits(body, at, d.counted)
	w.align()
	w.copyBits(body, d.aligned, d.end)
}

// crc32MPEG2 is the CRC-32 of MPEG-2: polynomial 0x04C11DB7, most significant bit first, no final inversion.
func crc32MPEG2(b []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, c := range b {
		crc ^= uint32(c) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
