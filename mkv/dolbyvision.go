package mkv

import "fmt"

// dolbyvision.go - the Dolby Vision configuration shared by the MP4 (dvcC/dvvC
// box) and Matroska (dvcC/dvvC BlockAddIDExtraData) readers. Both carry the same
// DOVIDecoderConfigurationRecord, so a single decoder serves both and a probe can
// report dovi_profile / bl_signal_compatibility_id without falling back to
// any external prober - the fields a player needs to pick a Dolby Vision rendering path.

// DolbyVision is a decoded DOVIDecoderConfigurationRecord. Profile and
// BLSignalCompatID are what a remux/playback decision keys on: profile selects the
// Dolby Vision flavour (e.g. 5 = single-layer, 8 = cross-compatible) and
// BLSignalCompatID says what the base layer is compatible with (1 = HDR10,
// 2 = SDR, 4 = HLG, 0 = none).
type DolbyVision struct {
	VersionMajor     uint8 `json:"version_major"`
	VersionMinor     uint8 `json:"version_minor"`
	Profile          uint8 `json:"profile"`                    // dv_profile (dovi_profile)
	Level            uint8 `json:"level"`                      // dv_level
	RPUPresent       bool  `json:"rpu_present"`                // rpu_present_flag
	ELPresent        bool  `json:"el_present"`                 // el_present_flag
	BLPresent        bool  `json:"bl_present"`                 // bl_present_flag
	BLSignalCompatID uint8 `json:"bl_signal_compatibility_id"` // dv_bl_signal_compatibility_id
	// EnhancementLayer is what a profile 7 track's enhancement layer holds, read from the first sample's RPU on request
	// (reader.WithDolbyVisionLayer, mp4 Options.DolbyVisionLayer); "" when not read or not stated.
	EnhancementLayer DolbyVisionEL `json:"enhancement_layer,omitempty"`
}

// DolbyVisionEL names the content of a Dolby Vision profile 7 enhancement layer.
type DolbyVisionEL string

// A minimal enhancement layer adds nothing to the picture, so dropping it loses none; a full one holds a residual
// the base layer alone, or a conversion to profile 8.1, no longer shows.
const (
	DolbyVisionELUnknown DolbyVisionEL = ""
	DolbyVisionMEL       DolbyVisionEL = "MEL"
	DolbyVisionFEL       DolbyVisionEL = "FEL"
)

// ParseDolbyVisionConfig decodes a DOVIDecoderConfigurationRecord - the payload of
// an MP4 dvcC/dvvC box or a Matroska dvcC/dvvC BlockAddIDExtraData. The fields it
// reads occupy the first five bytes:
//
//	dv_version_major (8) dv_version_minor (8)
//	dv_profile (7) dv_level (6) rpu(1) el(1) bl(1)
//	dv_bl_signal_compatibility_id (4) reserved…
//
// It returns nil when the record is shorter than five bytes.
func ParseDolbyVisionConfig(b []byte) *DolbyVision {
	if len(b) < 5 {
		return nil
	}
	return &DolbyVision{
		VersionMajor:     b[0],
		VersionMinor:     b[1],
		Profile:          b[2] >> 1,
		Level:            (b[2]&0x01)<<5 | b[3]>>3,
		RPUPresent:       b[3]&0x04 != 0,
		ELPresent:        b[3]&0x02 != 0,
		BLPresent:        b[3]&0x01 != 0,
		BLSignalCompatID: b[4] >> 4,
	}
}

// EncodeDolbyVisionConfig builds a 24-byte DOVIDecoderConfigurationRecord from dv
// (the inverse of ParseDolbyVisionConfig), so the configuration can be carried
// through a remux - into an MP4 dvcC/dvvC box or a Matroska BlockAddIDExtraData.
func EncodeDolbyVisionConfig(dv *DolbyVision) []byte {
	b := make([]byte, 24)
	b[0] = dv.VersionMajor
	b[1] = dv.VersionMinor
	b[2] = dv.Profile<<1 | (dv.Level>>5)&0x01
	b[3] = (dv.Level & 0x1f) << 3
	if dv.RPUPresent {
		b[3] |= 0x04
	}
	if dv.ELPresent {
		b[3] |= 0x02
	}
	if dv.BLPresent {
		b[3] |= 0x01
	}
	b[4] = dv.BLSignalCompatID << 4
	return b
}

// DolbyVisionBoxType returns the four-character config box / BlockAddIDType for a
// Dolby Vision profile: dvvC for the cross-compatible profiles (8 and up), dvcC
// otherwise. Returned as the uint32 used by Matroska's BlockAddIDType.
func (dv *DolbyVision) BoxType() (name string, blockAddIDType uint32) {
	if dv.Profile >= 8 {
		return "dvvC", BlockAddIDTypeDVVC
	}
	return "dvcC", BlockAddIDTypeDVCC
}

// DolbyVisionConversion is what a remux does to the layers of an HEVC Dolby Vision track, without re-encoding: the
// base layer is carried byte for byte under every value.
type DolbyVisionConversion int

// The Dolby Vision conversions.
const (
	// DolbyVisionKeep carries every layer as stored.
	DolbyVisionKeep DolbyVisionConversion = iota
	// DolbyVisionToProfile81 turns a dual-layer profile 7 track into a single-layer profile 8.1 one: the
	// enhancement layer is dropped and each RPU rewritten (residual off, a full enhancement layer's reshaping set to
	// the identity). A track of another profile is carried as stored.
	DolbyVisionToProfile81
	// DolbyVisionToBaseLayer drops the enhancement layer and the RPU of a profile 7 or 8 track, which leaves its
	// base layer alone - HDR10 for profiles 7 and 8.1 - with no Dolby Vision signal. A track of another profile is
	// carried as stored.
	DolbyVisionToBaseLayer
)

// Applies reports whether the conversion rewrites the samples of t.
func (c DolbyVisionConversion) Applies(t *Track) bool {
	if t == nil || t.DolbyVision == nil || t.Codec != "hevc" {
		return false
	}
	switch c {
	case DolbyVisionToProfile81:
		return t.DolbyVision.Profile == 7
	case DolbyVisionToBaseLayer:
		return t.DolbyVision.Profile == 7 || t.DolbyVision.Profile == 8
	}
	return false
}

// Converted returns the configuration the conversion leaves a track of configuration dv: nil when no Dolby Vision
// signal remains.
func (dv *DolbyVision) Converted(c DolbyVisionConversion) *DolbyVision {
	if c != DolbyVisionToProfile81 {
		return nil
	}
	return &DolbyVision{VersionMajor: dv.VersionMajor, VersionMinor: dv.VersionMinor, Profile: 8, Level: dv.Level,
		RPUPresent: true, BLPresent: true, BLSignalCompatID: 1}
}

// DolbyVisionError is returned by a Dolby Vision conversion for a track or a sample it cannot rewrite: the sample is
// never written as stored in its place.
type DolbyVisionError struct {
	Track  uint64 // the source track's number
	TimeMs int64  // the sample's time, 0 for the track as a whole
	Reason string
}

// Error names the track, the sample's time and the reason, and what to do.
func (e *DolbyVisionError) Error() string {
	return fmt.Sprintf("track %d: Dolby Vision conversion stopped at %d ms: %s (remux without the conversion to carry the layers as stored)", e.Track, e.TimeMs, e.Reason)
}
