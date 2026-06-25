package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestStereoProjectionMatroska covers reading the Matroska StereoMode and
// Projection (360/spherical) from the Video element.
func TestStereoProjectionMatroska(t *testing.T) {
	video := masterElem(mkv.IDVideo,
		uintElem(mkv.IDPixelWidth, 3840, 2), uintElem(mkv.IDPixelHeight, 2160, 2),
		uintElem(mkv.IDStereoMode, 1, 1),                                   // side by side
		masterElem(mkv.IDProjection, uintElem(mkv.IDProjectionType, 1, 1)), // equirectangular
	)
	track := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_MPEGH/ISO/HEVC"),
		video,
	)
	file := segmentMKV(infoElem(), masterElem(mkv.IDTracks, track), clusterElem())

	c, err := Read(context.Background(), bytes.NewReader(file), "x.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	tr := c.Tracks[0]
	if tr.StereoMode == nil || *tr.StereoMode != 1 {
		t.Errorf("StereoMode = %v, want 1", tr.StereoMode)
	}
	if got := tr.StereoModeName(); got != "side by side" {
		t.Errorf("StereoModeName = %q, want \"side by side\"", got)
	}
	if tr.Projection != "equirectangular" {
		t.Errorf("Projection = %q, want equirectangular", tr.Projection)
	}
}

// TestStereoModeMonoIsNil checks that an explicit mono StereoMode leaves the field
// nil (no stereo), so StereoModeName reports "".
func TestStereoModeMonoIsNil(t *testing.T) {
	video := masterElem(mkv.IDVideo,
		uintElem(mkv.IDPixelWidth, 1920, 2), uintElem(mkv.IDPixelHeight, 1080, 2),
		uintElem(mkv.IDStereoMode, 0, 1),
	)
	track := trackEntry(uintElem(mkv.IDTrackNumber, 1, 1), uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_MPEGH/ISO/HEVC"), video)
	file := segmentMKV(infoElem(), masterElem(mkv.IDTracks, track), clusterElem())

	c, err := Read(context.Background(), bytes.NewReader(file), "x.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if c.Tracks[0].StereoMode != nil {
		t.Errorf("mono StereoMode should be nil, got %v", *c.Tracks[0].StereoMode)
	}
}
