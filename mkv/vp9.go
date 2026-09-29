package mkv

// VP9Level returns the VP9 level code (10×major + minor: 21 = level 2.1,
// 40 = level 4.0) for a w×h picture: the smallest level whose
// MaxLumaPictureSize (VP9 spec Annex A) fits it. A VP9 bitstream carries no
// level of its own - the level exists only in a container declaration (the
// MP4 vpcC, the vp09.PP.LL.DD codec string) - so this derivation is what any
// tool reports for a VP9 stream, and it is the level mkvgo's own MP4 remux
// writes into the vpcC. Picture size is the dominant constraint; a
// frame-rate-based bump could only ever raise the level, so this conservative
// choice stays a valid, decodable declaration. nil dimensions give level 1.0.
func VP9Level(w, h *uint32) uint16 {
	var size uint64
	if w != nil && h != nil {
		size = uint64(*w) * uint64(*h)
	}
	for _, e := range []struct {
		code   uint16
		maxPic uint64
	}{
		{10, 36864}, {11, 73728}, {20, 122880}, {21, 245760},
		{30, 552960}, {31, 983040}, {40, 2228224}, {41, 2228224},
		{50, 8912896}, {51, 8912896}, {52, 8912896},
		{60, 35651584}, {61, 35651584},
	} {
		if size <= e.maxPic {
			return e.code
		}
	}
	return 62
}
