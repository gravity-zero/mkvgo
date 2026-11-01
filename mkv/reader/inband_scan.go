package reader

import "github.com/gravity-zero/mkvgo/mkv"

// maxScanSamples bounds how many samples of an H.264 track are read looking for a
// field-coded picture: an encoder that codes some pictures as frames rarely codes
// that many in a row.
const maxScanSamples = 16

// maxSliceHeader is how much of a slice NAL is unescaped to reach field_pic_flag:
// four Exp-Golomb values and frame_num fit in far less.
const maxSliceHeader = 64

// avcSliceLayout is what an H.264 slice header needs from its SPS to reach field_pic_flag.
type avcSliceLayout struct {
	nalLen         int
	frameNumBits   int
	separatePlanes bool
}

// avcFieldCodingLayout returns the slice layout of an avcC whose SPS allows field
// coding (frame_mbs_only_flag 0). ok is false when the record is malformed, holds
// no SPS, an SPS codes frames only, or two SPS lay their slices out differently -
// a slice could then not be read without its PPS.
func avcFieldCodingLayout(cp []byte) (l avcSliceLayout, ok bool) {
	n := 0
	wellFormed := forEachAVCSPS(cp, func(bc *bitstreamColour) bool {
		cur := avcSliceLayout{frameNumBits: bc.avcFrameNumBits, separatePlanes: bc.avcSeparatePlanes}
		if !bc.avcFieldCoding || cur.frameNumBits > 16 || (n > 0 && cur != l) {
			n = -1
			return true
		}
		l, n = cur, n+1
		return false
	})
	if !wellFormed || n <= 0 {
		return avcSliceLayout{}, false
	}
	l.nalLen = int(cp[4]&0x03) + 1
	return l, true
}

// avcFieldPicture reports whether a length-prefixed H.264 access unit holds a
// field-coded slice (field_pic_flag 1), which proves the stream interlaced.
func avcFieldPicture(frame []byte, l avcSliceLayout) bool {
	found := false
	forEachHEVCNAL(frame, l.nalLen, func(_ int, nal []byte) bool { // the walk is codec-neutral; the H.264 type is the low 5 bits
		if t := nal[0] & 0x1f; t != 1 && t != 5 { // coded slice, non-IDR or IDR
			return false
		}
		hdr := nal[1:]
		if len(hdr) > maxSliceHeader {
			hdr = hdr[:maxSliceHeader]
		}
		r := &bitReader{data: unescapeRBSP(hdr)}
		r.ue() // first_mb_in_slice
		r.ue() // slice_type
		r.ue() // pic_parameter_set_id
		if l.separatePlanes {
			r.bits(2) // colour_plane_id
		}
		r.bits(l.frameNumBits) // frame_num
		found = r.bit() == 1 && !r.err
		return found
	})
	return found
}

func isAVCCodec(codec string) bool {
	switch codec {
	case "h264", "avc", "V_MPEG4/ISO/AVC":
		return true
	}
	return false
}

// needsInBandScan reports whether t is an H.264 track whose scan type only its
// slices can settle: no scan type stated, and an SPS that allows field coding.
func needsInBandScan(t *mkv.Track) bool {
	if t.Type != mkv.VideoTrack || t.ScanType != "" || !isAVCCodec(t.Codec) {
		return false
	}
	_, ok := avcFieldCodingLayout(t.CodecPrivate)
	return ok
}

// applyInBandScan sets ScanType "interlaced" when the access unit holds a
// field-coded slice. A frame-coded one proves nothing (MBAFF codes fields inside
// frames), so the scan type otherwise stays unknown. No field order is inferred.
func applyInBandScan(t *mkv.Track, frame []byte) {
	if l, ok := avcFieldCodingLayout(t.CodecPrivate); ok && avcFieldPicture(frame, l) {
		t.ScanType = "interlaced"
	}
}
