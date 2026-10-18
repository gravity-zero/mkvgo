package ops

import (
	"image"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// TestPGSCompositeIsBounded pins that objects placed far apart are refused rather than composited: a forged set
// placing two 1x1 objects at (0,0) and (65535,65535) asked for a 65536x65536 picture, 17 GB.
func TestPGSCompositeIsBounded(t *testing.T) {
	dot := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	ds := &subtitle.PGSDisplaySet{ScreenW: 1920, ScreenH: 1080, Objects: []subtitle.PGSObject{{X: 0, Y: 0, Image: dot}, {X: 4097, Y: 0, Image: dot}}}
	if _, err := flattenPGSDisplaySet(ds, 0); err == nil {
		t.Error("objects 4098 pixels apart were composited onto one picture")
	}
	ds.Objects[1].X = 1900
	if cue, err := flattenPGSDisplaySet(ds, 0); err != nil || cue.Image.Bounds().Dx() != 1901 {
		t.Errorf("two lines on a 1080p plane: %v", err)
	}
}
