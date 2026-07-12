package mkv

// stereo.go - names for the 3D stereo arrangement and the video projection.

// StereoModeName maps the track's StereoMode to a human-readable arrangement
// (the wording ffprobe uses for its Stereo 3D side data), or "" for mono / no
// stereo / an unknown value. The Matroska StereoMode enum pairs left-eye-first
// and right-eye-first variants of each layout; both map to the same name here.
func (t *Track) StereoModeName() string {
	if t.StereoMode == nil {
		return ""
	}
	switch *t.StereoMode {
	case 0:
		return "mono"
	case 1, 11:
		return "side by side"
	case 2, 3:
		return "top and bottom"
	case 4, 5:
		return "checkerboard"
	case 6, 7:
		return "row interleaved"
	case 8, 9:
		return "column interleaved"
	case 10:
		return "anaglyph (cyan/red)"
	case 12:
		return "anaglyph (green/magenta)"
	case 13, 14:
		return "block laced"
	}
	return ""
}

// projectionTypeName maps a Matroska ProjectionType value to a name; "" unknown.
func projectionTypeName(v uint64) string {
	switch v {
	case 0:
		return "rectangular"
	case 1:
		return "equirectangular"
	case 2:
		return "cubemap"
	case 3:
		return "mesh"
	}
	return ""
}

// ProjectionTypeName is projectionTypeName, exported for the readers that decode
// the raw container value (Matroska ProjectionType, MP4 sv3d) into Track.Projection.
func ProjectionTypeName(v uint64) string { return projectionTypeName(v) }
