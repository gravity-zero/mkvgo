package mkv

// dolbyvision.go — the Dolby Vision configuration shared by the MP4 (dvcC/dvvC
// box) and Matroska (dvcC/dvvC BlockAddIDExtraData) readers. Both carry the same
// DOVIDecoderConfigurationRecord, so a single decoder serves both and a probe can
// report dovi_profile / bl_signal_compatibility_id without falling back to
// ffprobe — the fields a player needs to pick a Dolby Vision rendering path.

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
}

// ParseDolbyVisionConfig decodes a DOVIDecoderConfigurationRecord — the payload of
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
