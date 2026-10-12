package ops

// keyframe_access.go - is a Matroska "keyframe" really a picture a decoder can
// start on? The block flag is the muxer's word, and a trickplay strip built on
// it decodes every sample alone: one that needs a reference picture comes out
// grey or broken, or the decoder drops it and the strip shifts by one tile from
// there on. So each sample is checked on its own bitstream before it is handed
// out: H.264 IDR or all-intra slices, HEVC IRAP, AV1 and VP8/VP9 key frames.

import "errors"

// Access point kinds a batch sample reports (KeyframeBatchSample.Access).
const (
	AccessIDR = "idr" // H.264/HEVC instantaneous decoder refresh
	AccessCRA = "cra" // HEVC clean random access
	AccessBLA = "bla" // HEVC broken link access
	// AccessIntra is an H.264 picture whose slices are all intra but that is
	// not an IDR (open-GOP Blu-ray style): it decodes alone, bit-exact.
	AccessIntra = "i"
	AccessKey   = "key" // VP8/VP9/AV1 key frame
)

// errNotAccessPoint is a sample the muxer flagged as a keyframe whose
// bitstream says otherwise: it cannot be decoded on its own.
var errNotAccessPoint = errors.New("keyframe flag on a picture that is not a random access point")

// accessPoint classifies one decoded (decompressed, still length-prefixed for
// H.264/HEVC) sample of track codec. hasSeq reports whether an AV1 sample
// carries its own sequence header OBU.
func accessPoint(codec string, nalLen int, data []byte) (kind string, hasSeq bool, err error) {
	switch codec {
	case "h264":
		kind, err = avcAccess(data, nalLen)
	case "hevc":
		kind, err = hevcAccess(data, nalLen)
	case "av1":
		return av1Access(data)
	case "vp9":
		kind, err = vp9Access(data)
	case "vp8":
		kind, err = vp8Access(data)
	default:
		err = ErrUnsupportedCodec
	}
	return kind, false, err
}

// forEachNAL calls fn with each length-prefixed NAL unit of data.
func forEachNAL(data []byte, lenSize int, fn func(nal []byte) bool) error {
	if lenSize < 1 || lenSize > 4 {
		return errNotAccessPoint
	}
	for pos := 0; pos < len(data); {
		if pos+lenSize > len(data) {
			return errNotAccessPoint
		}
		n := 0
		for i := 0; i < lenSize; i++ {
			n = n<<8 | int(data[pos+i])
		}
		pos += lenSize
		if n <= 0 || n > len(data)-pos {
			return errNotAccessPoint
		}
		if !fn(data[pos : pos+n]) {
			return nil
		}
		pos += n
	}
	return nil
}

// avcAccess: an IDR slice makes the picture an IDR; otherwise every slice must
// be I or SI (slice_type % 5 == 2 or 4).
func avcAccess(data []byte, lenSize int) (string, error) {
	idr, intra, other := false, false, false
	err := forEachNAL(data, lenSize, func(nal []byte) bool {
		switch nal[0] & 0x1F {
		case 5:
			idr = true
		case 1:
			// slice_header: first_mb_in_slice ue(v), slice_type ue(v).
			var buf [16]byte
			r := expGolomb{data: rbspPrefix(buf[:0], nal[1:])}
			r.ue()
			st := r.ue()
			switch {
			case r.err:
				other = true
			case st%5 == 2 || st%5 == 4:
				intra = true
			default:
				other = true
			}
		}
		return true
	})
	switch {
	case err != nil:
		return "", err
	case idr:
		return AccessIDR, nil
	case intra && !other:
		return AccessIntra, nil
	}
	return "", errNotAccessPoint
}

// hevcAccess: the first VCL NAL of the picture must be an IRAP (16..23).
// Layered NALs (nuh_layer_id > 0, a Dolby Vision enhancement layer) are not
// the base picture and are passed over.
func hevcAccess(data []byte, lenSize int) (string, error) {
	kind := ""
	found := false
	err := forEachNAL(data, lenSize, func(nal []byte) bool {
		if len(nal) < 2 {
			return true
		}
		t := (nal[0] >> 1) & 0x3F
		layer := (nal[0]&1)<<5 | nal[1]>>3
		if t >= 32 || layer != 0 {
			return true
		}
		found = true
		switch {
		case t >= 16 && t <= 18:
			kind = AccessBLA
		case t == 19 || t == 20:
			kind = AccessIDR
		case t >= 21 && t <= 23:
			kind = AccessCRA
		}
		return false
	})
	if err != nil {
		return "", err
	}
	if !found || kind == "" {
		return "", errNotAccessPoint
	}
	return kind, nil
}

// vp9Access: frame_marker, profile, show_existing_frame = 0, frame_type = KEY,
// show_frame = 1. A superframe's index sits at its end; its first frame
// starts the buffer, which is the one read here.
func vp9Access(data []byte) (string, error) {
	r := expGolomb{data: data}
	if r.bits(2) != 2 {
		return "", errNotAccessPoint
	}
	profile := r.bits(1) | r.bits(1)<<1
	if profile == 3 {
		r.bits(1)
	}
	if r.bits(1) == 1 { // show_existing_frame
		return "", errNotAccessPoint
	}
	frameType, show := r.bits(1), r.bits(1)
	if r.err || frameType != 0 || show != 1 {
		return "", errNotAccessPoint
	}
	return AccessKey, nil
}

// vp8Access: frame tag bit 0 is 0 for a key frame, bit 4 is show_frame.
func vp8Access(data []byte) (string, error) {
	if len(data) < 3 || data[0]&1 != 0 || data[0]&0x10 == 0 {
		return "", errNotAccessPoint
	}
	return AccessKey, nil
}

// AV1 OBU types.
const (
	obuSequenceHeader = 1
	obuFrameHeader    = 3
	obuFrame          = 6
)

// forEachOBU walks a temporal unit's OBUs (low-overhead format: every OBU but
// possibly the last carries its size). fn gets the type, the whole OBU and its
// payload.
func forEachOBU(data []byte, fn func(typ byte, obu, payload []byte) bool) error {
	for pos := 0; pos < len(data); {
		start := pos
		h := data[pos]
		if h&0x80 != 0 {
			return errNotAccessPoint // forbidden bit
		}
		typ := (h >> 3) & 0x0F
		pos++
		if h&0x04 != 0 { // extension header
			pos++
		}
		if pos > len(data) {
			return errNotAccessPoint
		}
		size := len(data) - pos
		if h&0x02 != 0 {
			v, n := leb128(data[pos:])
			if n == 0 || v > uint64(len(data)-pos-n) {
				return errNotAccessPoint
			}
			pos += n
			size = int(v)
		}
		if !fn(typ, data[start:pos+size], data[pos:pos+size]) {
			return nil
		}
		pos += size
	}
	return nil
}

func leb128(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < 8 && i < len(b); i++ {
		v |= uint64(b[i]&0x7F) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

// av1Access: the temporal unit's first frame header must be a shown
// KEY_FRAME. A reduced_still_picture_header sequence is all key frames.
func av1Access(data []byte) (string, bool, error) {
	hasSeq, reduced := false, false
	kind := ""
	err := forEachOBU(data, func(typ byte, _, payload []byte) bool {
		switch typ {
		case obuSequenceHeader:
			hasSeq = true
			reduced = av1ReducedStill(payload)
		case obuFrameHeader, obuFrame:
			if reduced {
				kind = AccessKey
				return false
			}
			r := expGolomb{data: payload}
			if r.bits(1) == 1 { // show_existing_frame
				return false
			}
			frameType, show := r.bits(2), r.bits(1)
			if !r.err && frameType == 0 && show == 1 {
				kind = AccessKey
			}
			return false
		}
		return true
	})
	if err != nil {
		return "", hasSeq, err
	}
	if kind == "" {
		return "", hasSeq, errNotAccessPoint
	}
	return kind, hasSeq, nil
}

// av1ReducedStill reads reduced_still_picture_header from a sequence header
// OBU payload: seq_profile(3) still_picture(1) reduced_still_picture_header(1).
func av1ReducedStill(seq []byte) bool {
	return len(seq) > 0 && seq[0]&0x08 != 0
}

// av1SequenceHeaderOBU returns the sequence header OBU stored in an av1C
// record's configOBUs (after its 4-byte header), nil when there is none.
func av1SequenceHeaderOBU(av1C []byte) []byte {
	if len(av1C) <= 4 {
		return nil
	}
	var seq []byte
	_ = forEachOBU(av1C[4:], func(typ byte, obu, _ []byte) bool {
		if typ == obuSequenceHeader {
			seq = obu
			return false
		}
		return true
	})
	return seq
}

// expGolomb is a bounds-checked MSB-first bit reader with ue(v).
type expGolomb struct {
	data []byte
	pos  int
	err  bool
}

func (r *expGolomb) bits(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		if r.pos >= len(r.data)*8 {
			r.err = true
			return 0
		}
		v = v<<1 | uint32(r.data[r.pos>>3]>>(7-uint(r.pos&7))&1)
		r.pos++
	}
	return v
}

func (r *expGolomb) ue() uint32 {
	zeros := 0
	for r.bits(1) == 0 {
		if r.err || zeros >= 31 {
			r.err = true
			return 0
		}
		zeros++
	}
	return (1<<zeros - 1) + r.bits(zeros)
}

func (r *expGolomb) se() int32 {
	v := r.ue()
	if v&1 == 1 {
		return int32(v/2 + 1)
	}
	return -int32(v / 2)
}

// rbspPrefix appends to out (up to its capacity) the start of a NAL payload
// with the emulation prevention bytes (00 00 03) removed - enough for the
// first header fields.
func rbspPrefix(out, b []byte) []byte {
	zeros := 0
	for _, c := range b {
		if len(out) == cap(out) {
			break
		}
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}
