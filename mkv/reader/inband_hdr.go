package reader

import "github.com/gravity-zero/mkvgo/mkv"

// inband_hdr.go - HDR10 static metadata recovered from the first sample.
//
// The Content Light Level and the Mastering Display colour volume travel in two
// places: the container (Matroska Colour element, MP4 clli/mdcv boxes) and the
// bitstream itself (HEVC SEI messages 144 and 137, AV1 metadata OBUs HDR_CLL
// and HDR_MDCV). Many real HDR10 muxes write only the in-band copy: a head-only
// read then sees PQ colour and no static metadata, and a packaged init carries
// no mdcv/clli for it. Under WithInBandColourFallback, a PQ/HLG video track
// whose static metadata is incomplete reads its first sample - the same bounded
// read the colour fallback makes - and fills the missing part from the
// bitstream. The container keeps precedence per part: what it states is never
// overwritten.

// needsInBandHDR reports whether t is an HDR (PQ or HLG) HEVC or AV1 video
// track whose static metadata is incomplete - no Mastering Display, or no
// Content Light Level - so the first sample may complete it. A track whose
// transfer is still unknown is not selected here: the colour fallback reads its
// sample anyway, and the HDR pass runs on it once the SPS has named the transfer.
func needsInBandHDR(t *mkv.Track) bool {
	if t.Type != mkv.VideoTrack || t.ColorTransfer == nil {
		return false
	}
	if *t.ColorTransfer != 16 && *t.ColorTransfer != 18 { // smpte2084 (PQ) / arib-std-b67 (HLG)
		return false
	}
	if !isHEVCCodec(t.Codec) && !isAV1Codec(t.Codec) {
		return false
	}
	return !t.HDR.HasMasteringDisplay() || !t.HDR.HasContentLightLevel()
}

func isAV1Codec(codec string) bool {
	switch codec {
	case "av1", "V_AV1":
		return true
	default:
		return false
	}
}

// applyInBandHDR fills the parts of t.HDR the container left unknown from the
// first sample's static-metadata messages: HEVC SEI (prefix or suffix) payload
// types 137 (mastering_display_colour_volume) and 144 (content_light_level_info),
// AV1 metadata OBUs HDR_MDCV (2) and HDR_CLL (1). Safe on any input; an
// unparseable sample leaves t.HDR as it was.
func applyInBandHDR(t *mkv.Track, frame []byte) {
	var found mkv.HDRStaticMetadata
	switch {
	case isHEVCCodec(t.Codec):
		nalLen := 4
		if len(t.CodecPrivate) >= 22 {
			nalLen = int(t.CodecPrivate[21]&0x03) + 1
		}
		hdrFromHEVCFrame(frame, nalLen, &found)
	case isAV1Codec(t.Codec):
		hdrFromAV1TU(frame, &found)
	default:
		return
	}
	mergeInBandHDR(t, &found)
}

// mergeInBandHDR copies into t.HDR each part the container did not state.
func mergeInBandHDR(t *mkv.Track, found *mkv.HDRStaticMetadata) {
	if found.HasMasteringDisplay() && !t.HDR.HasMasteringDisplay() {
		ensureHDR(t).MasteringDisplay = found.MasteringDisplay
	}
	if found.HasContentLightLevel() && !t.HDR.HasContentLightLevel() {
		h := ensureHDR(t)
		h.MaxCLL, h.MaxFALL = found.MaxCLL, found.MaxFALL
	}
}

// --- HEVC ----------------------------------------------------------------------

// hdrFromHEVCFrame scans the SEI NAL units of a length-prefixed access unit for
// the two HDR10 static-metadata messages (ITU-T H.265 D.2.28 and D.2.35).
func hdrFromHEVCFrame(frame []byte, nalLen int, out *mkv.HDRStaticMetadata) {
	forEachHEVCNAL(frame, nalLen, func(nt int, nal []byte) bool {
		if nt != 39 && nt != 40 { // PREFIX_SEI_NUT / SUFFIX_SEI_NUT
			return false
		}
		forEachSEIMessage(unescapeRBSP(nal[2:]), func(pt int, payload []byte) {
			switch pt {
			case 137:
				if md := masteringFromSEI(payload); md != nil {
					out.MasteringDisplay = md
				}
			case 144:
				if len(payload) >= 4 {
					out.MaxCLL = uint32(be16(payload))
					out.MaxFALL = uint32(be16(payload[2:]))
				}
			}
		})
		return out.HasMasteringDisplay() && out.HasContentLightLevel()
	})
}

// masteringFromSEI decodes a mastering_display_colour_volume payload: the three
// primaries in G, B, R order then the white point, each (x, y) in units of
// 0.00002; then the max and min display luminance in units of 0.0001 cd/m².
func masteringFromSEI(p []byte) *mkv.MasteringDisplay {
	if len(p) < 24 {
		return nil
	}
	chroma := func(off int) float64 { return float64(be16(p[off:])) / 50000 }
	return &mkv.MasteringDisplay{
		GreenX: chroma(0), GreenY: chroma(2),
		BlueX: chroma(4), BlueY: chroma(6),
		RedX: chroma(8), RedY: chroma(10),
		WhiteX: chroma(12), WhiteY: chroma(14),
		LuminanceMax: float64(be32(p[16:])) / 10000,
		LuminanceMin: float64(be32(p[20:])) / 10000,
	}
}

// forEachSEIMessage walks the messages of an SEI RBSP (ff-coded payload type and
// size, ITU-T H.265 7.3.5), calling fn(payloadType, payload) for each; it stops
// at the first truncated message or at the rbsp_trailing_bits.
func forEachSEIMessage(rbsp []byte, fn func(pt int, payload []byte)) {
	for i := 0; i+1 < len(rbsp); { // need at least a type and a size byte
		pt, ok := readSEIValue(rbsp, &i)
		if !ok {
			return
		}
		ps, ok := readSEIValue(rbsp, &i)
		if !ok || i+ps > len(rbsp) {
			return
		}
		fn(pt, rbsp[i:i+ps])
		i += ps
	}
}

func be16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }
func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// --- AV1 -----------------------------------------------------------------------

// hdrFromAV1TU scans the OBUs of a temporal unit for the HDR_CLL and HDR_MDCV
// metadata OBUs (AV1 spec 5.8.3 / 6.7.3-6.7.4).
func hdrFromAV1TU(tu []byte, out *mkv.HDRStaticMetadata) {
	forEachAV1OBU(tu, func(obuType int, payload []byte) bool {
		if obuType != 5 { // OBU_METADATA
			return false
		}
		mt, n, ok := leb128(payload)
		if !ok {
			return false
		}
		p := payload[n:]
		switch mt {
		case 1: // METADATA_TYPE_HDR_CLL
			if len(p) >= 4 {
				out.MaxCLL = uint32(be16(p))
				out.MaxFALL = uint32(be16(p[2:]))
			}
		case 2: // METADATA_TYPE_HDR_MDCV
			if md := masteringFromAV1(p); md != nil {
				out.MasteringDisplay = md
			}
		}
		return out.HasMasteringDisplay() && out.HasContentLightLevel()
	})
}

// masteringFromAV1 decodes an HDR_MDCV payload: the three primaries in R, G, B
// order then the white point, each (x, y) as 0.16 fixed point; luminance_max as
// 24.8 and luminance_min as 18.14 fixed point, in cd/m².
func masteringFromAV1(p []byte) *mkv.MasteringDisplay {
	if len(p) < 24 {
		return nil
	}
	chroma := func(off int) float64 { return float64(be16(p[off:])) / 65536 }
	return &mkv.MasteringDisplay{
		RedX: chroma(0), RedY: chroma(2),
		GreenX: chroma(4), GreenY: chroma(6),
		BlueX: chroma(8), BlueY: chroma(10),
		WhiteX: chroma(12), WhiteY: chroma(14),
		LuminanceMax: float64(be32(p[16:])) / 256,
		LuminanceMin: float64(be32(p[20:])) / 16384,
	}
}

// forEachAV1OBU walks the OBUs of a buffer (sized OBUs, as a Matroska/MP4 sample
// stores them; an unsized OBU extends to the end), calling fn(obu_type, payload)
// for each; fn returns true to stop. Malformed sizes end the walk.
func forEachAV1OBU(b []byte, fn func(obuType int, payload []byte) bool) {
	for i := 0; i < len(b); {
		hdr := b[i]
		i++
		obuType := int(hdr>>3) & 0xf
		if hdr&0x04 != 0 { // obu_extension_flag
			if i >= len(b) {
				return
			}
			i++
		}
		size := len(b) - i
		if hdr&0x02 != 0 { // obu_has_size_field
			sz, n, ok := leb128(b[i:])
			if !ok {
				return
			}
			i += n
			size = int(sz)
		}
		if size < 0 || i+size > len(b) {
			return
		}
		if fn(obuType, b[i:i+size]) {
			return
		}
		i += size
	}
}
