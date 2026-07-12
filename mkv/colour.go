package mkv

// Colour-metadata helpers. The Matroska Colour element (0x55B0) stores its
// fields as CICP / ITU-T H.273 code points - the same integers FFmpeg keeps in
// AVColorSpace / AVColorTransferCharacteristic / AVColorPrimaries / AVColorRange.
// ffprobe prints them through av_color_*_name; the tables below reproduce those
// exact strings so a consumer can compare mkvgo's Track colour fields against
// `ffprobe -show_streams` output (color_space, color_transfer, color_primaries,
// color_range) without a second mapping table.
//
// A nil pointer on Track means the element was absent. An out-of-table value
// returns "" (unknown), mirroring how ffprobe omits the field.

// colorSpaceNames maps MatrixCoefficients (CICP) → ffprobe color_space.
var colorSpaceNames = map[uint16]string{
	0:  "gbr",
	1:  "bt709",
	2:  "unknown",
	4:  "fcc",
	5:  "bt470bg",
	6:  "smpte170m",
	7:  "smpte240m",
	8:  "ycgco",
	9:  "bt2020nc",
	10: "bt2020c",
	11: "smpte2085",
	12: "chroma-derived-nc",
	13: "chroma-derived-c",
	14: "ictcp",
}

// colorTransferNames maps TransferCharacteristics (CICP) → ffprobe color_transfer.
var colorTransferNames = map[uint16]string{
	1:  "bt709",
	2:  "unknown",
	4:  "bt470m",
	5:  "bt470bg",
	6:  "smpte170m",
	7:  "smpte240m",
	8:  "linear",
	9:  "log100",
	10: "log316",
	11: "iec61966-2-4",
	12: "bt1361e",
	13: "iec61966-2-1",
	14: "bt2020-10",
	15: "bt2020-12",
	16: "smpte2084", // PQ (HDR10 / Dolby Vision)
	17: "smpte428",
	18: "arib-std-b67", // HLG
}

// colorPrimariesNames maps Primaries (CICP) → ffprobe color_primaries.
var colorPrimariesNames = map[uint16]string{
	1:  "bt709",
	2:  "unknown",
	4:  "bt470m",
	5:  "bt470bg",
	6:  "smpte170m",
	7:  "smpte240m",
	8:  "film",
	9:  "bt2020",
	10: "smpte428",
	11: "smpte431",
	12: "smpte432",
	22: "ebu3213",
}

// colorRangeNames maps the Matroska/CICP Range value → ffprobe color_range.
// Matroska Range: 0 unspecified, 1 broadcast (limited), 2 full, 3 derived.
var colorRangeNames = map[uint16]string{
	1: "tv", // limited / broadcast (AVCOL_RANGE_MPEG)
	2: "pc", // full              (AVCOL_RANGE_JPEG)
}

func nameOf(table map[uint16]string, code *uint16) string {
	if code == nil {
		return ""
	}
	return table[*code]
}

// ColorSpaceName returns the ffprobe color_space string for t's MatrixCoefficients
// code point, or "" if absent/unknown.
func (t *Track) ColorSpaceName() string { return nameOf(colorSpaceNames, t.ColorSpace) }

// ColorTransferName returns the ffprobe color_transfer string for t's
// TransferCharacteristics code point, or "" if absent/unknown.
func (t *Track) ColorTransferName() string { return nameOf(colorTransferNames, t.ColorTransfer) }

// ColorPrimariesName returns the ffprobe color_primaries string for t's Primaries
// code point, or "" if absent/unknown.
func (t *Track) ColorPrimariesName() string { return nameOf(colorPrimariesNames, t.ColorPrimaries) }

// ColorRangeName returns the ffprobe color_range string ("tv"/"pc") for t's Range
// value, or "" if absent/unknown.
func (t *Track) ColorRangeName() string { return nameOf(colorRangeNames, t.ColorRange) }

// HDRFormat classifies a video track's dynamic-range signalling in one word -
// the field a serving decision keys on (tonemap or direct-play):
//
//	"dolby-vision" - a Dolby Vision configuration whose base layer is NOT
//	                 independently playable: any profile other than 8, and
//	                 profile 8 with an unknown compatibility id
//	"hdr10"        - PQ transfer (SMPTE ST 2084), or Dolby Vision profile 8
//	                 with an HDR10-compatible base layer (bl_signal_compatibility_id 1)
//	"hlg"          - HLG transfer (ARIB STD-B67), or DoVi profile 8 with an
//	                 HLG base layer (id 4)
//	"sdr"          - colour is known (or was determined) and none of the
//	                 above, or DoVi profile 8 with an SDR base layer (id 2)
//	""             - not a video track, or colour entirely unknown
//
// The Dolby Vision rule is deliberate: profile 8 is the cross-compatible
// flavour whose base layer plays WITHOUT a DoVi decoder, so the class that
// drives the decision is the base layer's (bl_signal_compatibility_id
// 1 = HDR10, 2 = SDR, 4 = HLG) - a profile-8/compat-1 title direct-plays on
// any HDR10 client. Only a stream that genuinely needs the DoVi rendering
// path (profile 5's IPT-PQ single layer, unknown compat) reports
// "dolby-vision". The raw DolbyVision fields ride alongside for consumers
// applying their own policy. Container metadata only (the same signals
// IsHDR reads); the transfer function alone decides the PQ/HLG split
// because it is what a tonemapping decision consumes.
func (t *Track) HDRFormat() string {
	if t.Type != VideoTrack {
		return ""
	}
	if dv := t.DolbyVision; dv != nil {
		if dv.Profile == 8 {
			switch dv.BLSignalCompatID {
			case 1:
				return "hdr10"
			case 2:
				return "sdr"
			case 4:
				return "hlg"
			}
		}
		return "dolby-vision"
	}
	if t.ColorTransfer != nil {
		switch *t.ColorTransfer {
		case 16:
			return "hdr10"
		case 18:
			return "hlg"
		}
	}
	if t.ColorTransfer != nil || t.ColorPrimaries != nil || t.ColorSpace != nil || t.ColourDetermined {
		return "sdr"
	}
	return ""
}

// IsHDR reports whether the track's colour metadata indicates a high-dynamic-range
// video signal: BT.2020 primaries (9) combined with a PQ (16) or HLG (18) transfer
// function - the standard HDR10/HLG signalling. It is a best-effort heuristic from
// container metadata only (it does not inspect the bitstream) and returns false
// when the Colour element is absent.
func (t *Track) IsHDR() bool {
	if t.ColorPrimaries == nil || *t.ColorPrimaries != 9 {
		return false
	}
	if t.ColorTransfer == nil {
		return false
	}
	switch *t.ColorTransfer {
	case 16, 18: // SMPTE ST 2084 (PQ) or ARIB STD-B67 (HLG)
		return true
	default:
		return false
	}
}
