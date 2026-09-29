package reader

import (
	"fmt"
	"strconv"

	"github.com/gravity-zero/mkvgo/mkv"
)

// VP9KeyframeHeader holds the fields of a VP9 keyframe's uncompressed header
// that describe the stream: the ones a vpcC / vp09 codec string is built from.
type VP9KeyframeHeader struct {
	Profile  uint8 // 0..3
	BitDepth uint8 // 8, 10 or 12
	Chroma   uint8 // vpcC chromaSubsampling code: 0 = 4:2:0, 2 = 4:2:2, 3 = 4:4:4
	// ColorSpace is the header's 3-bit color_space: 0 unknown, 1 BT.601,
	// 2 BT.709, 3 SMPTE 170, 4 SMPTE 240, 5 BT.2020, 6 reserved, 7 sRGB.
	ColorSpace uint8
	FullRange  bool
}

// ParseVP9KeyframeHeader reads the start of a VP9 KEYFRAME's uncompressed
// header (VP9 bitstream spec §6.2): frame marker, profile, sync code, then the
// colour config that carries the bit depth and chroma subsampling. A
// non-keyframe, a wrong sync code or a truncated header is an error. The
// input is attacker-controlled (the first sample of any track labelled vp9);
// the bit reader is bounds-checked and never panics.
func ParseVP9KeyframeHeader(b []byte) (*VP9KeyframeHeader, error) {
	r := &bitReader{data: b}
	if r.bits(2) != 2 {
		return nil, fmt.Errorf("VP9: bad frame_marker")
	}
	profile := uint8(r.bits(1)) | uint8(r.bits(1))<<1
	if profile == 3 {
		r.bits(1) // reserved_zero
	}
	if r.bits(1) == 1 {
		return nil, fmt.Errorf("VP9: show_existing_frame, not a keyframe")
	}
	frameType := r.bits(1)
	r.bits(2) // show_frame, error_resilient_mode
	if frameType != 0 {
		return nil, fmt.Errorf("VP9: first sample is not a keyframe")
	}
	if r.bits(24) != 0x498342 {
		return nil, fmt.Errorf("VP9: bad frame sync code")
	}
	h := &VP9KeyframeHeader{Profile: profile, BitDepth: 8}
	if profile >= 2 {
		if r.bits(1) == 1 {
			h.BitDepth = 12
		} else {
			h.BitDepth = 10
		}
	}
	h.ColorSpace = uint8(r.bits(3))
	if h.ColorSpace != 7 { // 7 = CS_RGB
		h.FullRange = r.bits(1) == 1
		if profile == 1 || profile == 3 {
			sx, sy := r.bits(1), r.bits(1)
			r.bits(1) // reserved_zero
			switch {
			case sx == 1 && sy == 1:
				h.Chroma = 0 // 4:2:0
			case sx == 1:
				h.Chroma = 2 // 4:2:2
			default:
				h.Chroma = 3 // 4:4:4
			}
		} else {
			h.Chroma = 0 // profiles 0 and 2 are always 4:2:0
		}
	} else {
		h.FullRange = true
		h.Chroma = 3 // CS_RGB is 4:4:4
	}
	if r.err {
		return nil, fmt.Errorf("VP9: truncated frame header")
	}
	return h, nil
}

func isVP9Codec(codec string) bool {
	switch codec {
	case "vp9", "V_VP9":
		return true
	}
	return false
}

// vp9ChromaCode maps a vpcC chromaSubsampling code to the chroma index
// pixelFormat expects (1 = 4:2:0, 2 = 4:2:2, 3 = 4:4:4).
func vp9ChromaCode(code uint8) *uint16 {
	switch code {
	case 0, 1:
		return u16p(1)
	case 2:
		return u16p(2)
	case 3:
		return u16p(3)
	}
	return nil
}

// vp9ProfileName is the conventional prober spelling of a VP9 profile.
func vp9ProfileName(p uint8) string { return "Profile " + strconv.Itoa(int(p)) }

// vp9HeaderColour turns a parsed keyframe header into the bitstream-derived
// fields mergeBitstreamColour fills gaps with: profile, bit depth, chroma
// (hence pixel format), range, and the colour the header's color_space names.
//
// color_space is one named standard, not three CICP code points, so a code
// point is filled only where that standard fixes it beyond doubt (H.273):
// BT.709 and sRGB fix all three; SMPTE 170M and 240M fix all three; BT.601
// fixes the matrix (BT.470BG, the mainstream decoder's reading) and the
// transfer (code 6 covers both the 525- and 625-line variants) but not the
// primaries, which differ between the two. BT.2020 fixes primaries and matrix
// but NOT the transfer: a VP9 HDR stream is BT.2020 with PQ or HLG, which the
// header cannot express and only the container states - inventing the SDR
// BT.2020 transfer there would turn an HDR stream into a wrongly-graded SDR
// one. unknown and reserved fill nothing.
func vp9HeaderColour(h *VP9KeyframeHeader) *bitstreamColour {
	bc := &bitstreamColour{
		profile:  vp9ProfileName(h.Profile),
		bitDepth: u16p(uint16(h.BitDepth)),
		chroma:   vp9ChromaCode(h.Chroma),
	}
	if h.FullRange {
		bc.rng = u16p(2)
	} else {
		bc.rng = u16p(1)
	}
	switch h.ColorSpace {
	case 1: // CS_BT_601
		bc.matrix, bc.transfer = u16p(5), u16p(6)
	case 2: // CS_BT_709
		bc.primaries, bc.transfer, bc.matrix = u16p(1), u16p(1), u16p(1)
	case 3: // CS_SMPTE_170
		bc.primaries, bc.transfer, bc.matrix = u16p(6), u16p(6), u16p(6)
	case 4: // CS_SMPTE_240
		bc.primaries, bc.transfer, bc.matrix = u16p(7), u16p(7), u16p(7)
	case 5: // CS_BT_2020 (non-constant luminance, the mainstream reading)
		bc.primaries, bc.matrix = u16p(9), u16p(9)
	case 7: // CS_RGB = sRGB (IEC 61966-2-1): identity matrix
		bc.primaries, bc.transfer, bc.matrix = u16p(1), u16p(13), u16p(0)
	}
	bc.determined = h.ColorSpace != 0 && h.ColorSpace != 6
	return bc
}

// fillVP9Level derives Track.Level for a VP9 video track the container gave no
// level for (no vpcC, or a vpcC declaring 0). VP9 carries no level in the
// bitstream; see mkv.VP9Level for why the picture-size derivation is the
// honest value and the one mkvgo's own MP4 remux declares.
func fillVP9Level(t *mkv.Track) {
	if t.Type != mkv.VideoTrack || t.Level != nil || !isVP9Codec(t.Codec) {
		return
	}
	t.Level = u16p(mkv.VP9Level(t.Width, t.Height))
}

// VP9FeatureMetadata is the Matroska "VP9 Codec Feature Metadata" form of a
// VP9 track's CodecPrivate (the Matroska codec specification for V_VP9, what
// mkvmerge writes): a sequence of one-byte-value records {id, length=1,
// value} with id 1 = Profile, 2 = Level, 3 = Bit depth, 4 = Chroma
// subsampling (0/1 = 4:2:0, 2 = 4:2:2, 3 = 4:4:4). It carries no colour. A
// field the file does not write is nil. It is NOT a VPCodecConfigurationRecord
// (vpcC), which some other muxers store in the same element - the two are
// told apart by ParseVP9FeatureMetadata's strict walk.
type VP9FeatureMetadata struct {
	Profile, Level, BitDepth, Chroma *uint8
}

// ParseVP9FeatureMetadata reports whether cp is a VP9 Codec Feature Metadata
// blob - every byte consumed by well-formed {id 1..4, length 1, value}
// records - and returns its fields. Any other layout (a vpcC, empty, junk)
// returns ok=false. The check is unambiguous against a vpcC: a bare vpcC's
// second byte is a level code (10..62, never 1) and a FullBox vpcC's is a
// zero flags byte, while a feature record's second byte is always 1.
func ParseVP9FeatureMetadata(cp []byte) (m VP9FeatureMetadata, ok bool) {
	if len(cp) == 0 {
		return m, false
	}
	for i := 0; i < len(cp); i += 3 {
		if i+3 > len(cp) || cp[i] < 1 || cp[i] > 4 || cp[i+1] != 1 {
			return VP9FeatureMetadata{}, false
		}
		v := cp[i+2]
		switch cp[i] {
		case 1:
			m.Profile = &v
		case 2:
			m.Level = &v
		case 3:
			m.BitDepth = &v
		case 4:
			m.Chroma = &v
		}
	}
	return m, true
}

// validVP9Level reports whether code is a VP9 level code (Annex A): a
// declaration outside the table (0, 1, 99...) is not a level and must not be
// reported as one - the picture-size derivation takes over.
func validVP9Level(code uint8) bool {
	switch code {
	case 10, 11, 20, 21, 30, 31, 40, 41, 50, 51, 52, 60, 61, 62:
		return true
	}
	return false
}

// vp9FeatureColour turns a feature-metadata blob into the bitstream-derived
// fields: profile, a valid level, bit depth, chroma (hence pixel format). No
// colour is carried, so nothing is "determined" by it.
func vp9FeatureColour(m VP9FeatureMetadata) *bitstreamColour {
	bc := &bitstreamColour{}
	if m.Profile != nil && *m.Profile <= 3 {
		bc.profile = vp9ProfileName(*m.Profile)
	}
	if m.Level != nil && validVP9Level(*m.Level) {
		bc.level = u16p(uint16(*m.Level))
	}
	if m.BitDepth != nil && (*m.BitDepth == 8 || *m.BitDepth == 10 || *m.BitDepth == 12) {
		bc.bitDepth = u16p(uint16(*m.BitDepth))
	}
	if m.Chroma != nil {
		bc.chroma = vp9ChromaCode(*m.Chroma)
	}
	return bc
}

// VP9FeatureMetadataFromVpcC converts a VPCodecConfigurationRecord (FullBox
// or bare form, as an MP4 vp09 entry carries it) into the Matroska VP9 Codec
// Feature Metadata records a V_VP9 CodecPrivate is specified to hold. The
// vpcC's colour is not carried - it belongs to the Matroska Colour element -
// and a level outside the level table is left out. ok is false when cp is
// not a vpcC (already feature metadata, empty, junk).
func VP9FeatureMetadataFromVpcC(cp []byte) (out []byte, ok bool) {
	if _, isFeature := ParseVP9FeatureMetadata(cp); isFeature {
		return nil, false
	}
	var b []byte
	switch {
	case len(cp) >= 12 && cp[0] <= 1:
		b = cp[4:]
	case len(cp) >= 8:
		b = cp
	default:
		return nil, false
	}
	if b[0] > 3 {
		return nil, false
	}
	out = append(out, 1, 1, b[0])
	if validVP9Level(b[1]) {
		out = append(out, 2, 1, b[1])
	}
	if d := b[2] >> 4; d == 8 || d == 10 || d == 12 {
		out = append(out, 3, 1, d)
	}
	out = append(out, 4, 1, (b[2]>>1)&0x7)
	return out, true
}
