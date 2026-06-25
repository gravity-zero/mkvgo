package mp4

import "testing"

// TestSpatialMP4 covers reading st3d (stereo, mapped to Matroska StereoMode) and
// sv3d (spherical projection) from a visual sample entry.
func TestSpatialMP4(t *testing.T) {
	st3d := box("st3d", []byte{0, 0, 0, 0, 2}) // version+flags + stereo_mode = 2 (left-right)
	sv3d := box("sv3d", box("proj", box("equi", make([]byte, 4))))

	payload := make([]byte, 78) // visual sample entry fixed header
	payload = append(payload, box("hvcC", []byte{1, 2, 3})...)
	payload = append(payload, st3d...)
	payload = append(payload, sv3d...)

	var tr inTrack
	parseSpatial(&tr, payload, 78)

	if tr.stereoMode == nil || *tr.stereoMode != 1 { // st3d left-right → Matroska side-by-side (1)
		t.Errorf("stereoMode = %v, want 1", tr.stereoMode)
	}
	if tr.projection != "equirectangular" {
		t.Errorf("projection = %q, want equirectangular", tr.projection)
	}

	// A flat entry (no st3d/sv3d) leaves both unset.
	var flat inTrack
	parseSpatial(&flat, append(make([]byte, 78), box("hvcC", []byte{1})...), 78)
	if flat.stereoMode != nil || flat.projection != "" {
		t.Errorf("flat entry: stereoMode=%v projection=%q, want unset", flat.stereoMode, flat.projection)
	}
}
