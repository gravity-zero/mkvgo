package mp4

// codecstring.go — RFC 6381 codec strings for the HLS master playlist's CODECS
// attribute, derived from each track's configuration record. Returns "" when a
// codec's string cannot be produced; the playlist then omits the attribute
// entirely (a partial CODECS list is worse than none).

import "fmt"

// rfc6381Codec returns the codec string for one output track, or "".
func rfc6381Codec(t *outTrack) string {
	cp := t.mkv.CodecPrivate
	switch t.mkv.Codec {
	case "h264":
		// avcC: configurationVersion, AVCProfileIndication, profile_compatibility,
		// AVCLevelIndication — the three bytes the string carries verbatim.
		if len(cp) < 4 {
			return ""
		}
		return fmt.Sprintf("avc1.%02X%02X%02X", cp[1], cp[2], cp[3])
	case "hevc":
		return hevcCodecString(cp)
	case "av1":
		return av1CodecString(cp)
	case "vp9":
		if rec := vpcCRecord(cp); len(rec) >= 3 {
			// vp09.PP.LL.DD (profile, level, bit depth), zero-padded.
			return fmt.Sprintf("vp09.%02d.%02d.%02d", rec[0], rec[1], rec[2]>>4)
		}
		return ""
	case "aac":
		// mp4a.40.<AudioObjectType> from the AudioSpecificConfig.
		if len(cp) == 0 {
			return ""
		}
		r := &bitReader{data: cp}
		aot := getAudioObjectType(r)
		if r.err || aot == 0 {
			return ""
		}
		return fmt.Sprintf("mp4a.40.%d", aot)
	case "A_MPEG/L3":
		return "mp4a.40.34"
	case "ac3":
		return "ac-3"
	case "eac3":
		return "ec-3"
	case "opus":
		return "opus"
	case "flac":
		return "flac"
	}
	return ""
}

// hevcCodecString builds the ISO/IEC 14496-15 annex E string from an hvcC:
// hvc1.<profile_space?><profile_idc>.<compat, hex of bit-reversed flags>.
// <T|H|L><level_idc>.<constraint bytes, trailing zeros trimmed>.
func hevcCodecString(hvcC []byte) string {
	if len(hvcC) < 13 {
		return ""
	}
	profileSpace := hvcC[1] >> 6
	tier := (hvcC[1] >> 5) & 1
	profileIDC := hvcC[1] & 0x1F
	compat := uint32(hvcC[2])<<24 | uint32(hvcC[3])<<16 | uint32(hvcC[4])<<8 | uint32(hvcC[5])
	levelIDC := hvcC[12]

	s := "hvc1."
	if profileSpace > 0 {
		s += string(rune('A' + profileSpace - 1))
	}
	s += fmt.Sprintf("%d.%X.", profileIDC, bitReverse32(compat))
	if tier == 1 {
		s += "H"
	} else {
		s += "L"
	}
	s += fmt.Sprintf("%d", levelIDC)
	// Constraint bytes 6..11, trailing zero bytes omitted.
	constraints := hvcC[6:12]
	last := len(constraints)
	for last > 0 && constraints[last-1] == 0 {
		last--
	}
	for _, b := range constraints[:last] {
		s += fmt.Sprintf(".%X", b)
	}
	return s
}

func bitReverse32(v uint32) uint32 {
	var out uint32
	for i := 0; i < 32; i++ {
		out = out<<1 | (v>>i)&1
	}
	return out
}

// av1CodecString builds av01.P.LLT.DD from an av1C record (marker/version,
// then seq_profile(3)+seq_level_idx(5), then tier/bitdepth flags).
func av1CodecString(av1C []byte) string {
	if len(av1C) < 3 {
		return ""
	}
	profile := av1C[1] >> 5
	level := av1C[1] & 0x1F
	tier := "M"
	if av1C[2]>>7 == 1 {
		tier = "H"
	}
	depth := 8
	highBD := (av1C[2] >> 6) & 1
	twelve := (av1C[2] >> 5) & 1
	if highBD == 1 {
		depth = 10
		if twelve == 1 {
			depth = 12
		}
	}
	return fmt.Sprintf("av01.%d.%02d%s.%02d", profile, level, tier, depth)
}

// hlsCodecsAttr returns the CODECS attribute value for a set of tracks, or ""
// when any track's string is unknown.
func hlsCodecsAttr(tracks []*fragTrack) string {
	var out string
	for _, ft := range tracks {
		cs := rfc6381Codec(ft.outTrack)
		if cs == "" {
			return ""
		}
		if out != "" {
			out += ","
		}
		out += cs
	}
	return out
}
