package mkv

import "testing"

// ffmpegOrientations are ffmpeg's outputs for -display_rotation R with
// -display_hflip / -display_vflip: the display matrix (a, b, c, d, in units of
// 1.0) it writes to an MP4 tkhd and a Matroska file, and the Matroska
// ProjectionPoseYaw and ProjectionPoseRoll it writes.
var ffmpegOrientations = []struct {
	rot        int // -display_rotation, counterclockwise
	h, v       bool
	a, b, c, d float64
	yaw, roll  float64
}{
	{0, true, false, -1, 0, 0, 1, 180, 0},
	{0, false, true, 1, 0, 0, -1, 180, 180},
	{0, true, true, -1, 0, 0, -1, 0, 180},
	{90, false, false, 0, -1, 1, 0, 0, 90},
	{90, true, false, 0, -1, -1, 0, 180, -90},
	{90, false, true, 0, 1, 1, 0, 180, 90},
	{90, true, true, 0, 1, -1, 0, 0, -90},
	{180, false, false, -1, 0, 0, -1, 0, 180},
	{180, true, false, 1, 0, 0, -1, 180, 180},
	{180, false, true, -1, 0, 0, 1, 180, 0},
	{270, false, false, 0, 1, -1, 0, 0, -90},
	{270, true, false, 0, 1, 1, 0, 180, 90},
	{270, false, true, 0, -1, -1, 0, 180, -90},
	{270, true, true, 0, -1, 1, 0, 0, 90},
}

// TestDegenerateMatrixIsNoRotation: a matrix whose first row has no length (a
// metadata track's 0 1e-5 0 0) is not a rotation; ffmpeg ignores it too.
func TestDegenerateMatrixIsNoRotation(t *testing.T) {
	if rot, hflip := OrientationFromMatrix(0, 1.52587890625e-05, 0, 0); rot != 0 || hflip {
		t.Errorf("degenerate matrix read as %d° hflip=%v, want 0 and no mirror", rot, hflip)
	}
	if rot, _ := OrientationFromMatrix(0, 1, -1, 0); rot != 90 {
		t.Errorf("a 90° matrix reads %d°", rot)
	}
}

// TestOrientationAsFFmpegWritesIt: every rotation with or without a mirror
// reads from ffmpeg's display matrix and from its Matroska pose to the same
// orientation, and writes back the matrix and the pose ffmpeg wrote.
func TestOrientationAsFFmpegWritesIt(t *testing.T) {
	for _, c := range ffmpegOrientations {
		rot, hflip := OrientationFromMatrix(c.a, c.b, c.c, c.d)
		if a, b, cc, d := OrientationMatrix(rot, hflip); a != c.a || b != c.b || cc != c.c || d != c.d {
			t.Errorf("rotation %d h=%v v=%v: read as %d° hflip=%v, matrix written back %v %v %v %v, want %v %v %v %v",
				c.rot, c.h, c.v, rot, hflip, a, b, cc, d, c.a, c.b, c.c, c.d)
		}
		zero := uint64(0)
		if pr, ph := OrientationFromProjection(&zero, c.yaw, 0, c.roll); pr != rot || ph != hflip {
			t.Errorf("rotation %d h=%v v=%v: pose yaw %v roll %v reads %d° hflip=%v, the matrix %d° hflip=%v", c.rot, c.h, c.v, c.yaw, c.roll, pr, ph, rot, hflip)
		}
		if yaw, roll := ProjectionPose(rot, hflip); yaw != c.yaw || roll != c.roll {
			t.Errorf("rotation %d h=%v v=%v: pose written yaw %v roll %v, want %v %v", c.rot, c.h, c.v, yaw, roll, c.yaw, c.roll)
		}
	}
}
