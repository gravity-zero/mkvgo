package mkv

import "math"

// OrientationFromMatrix returns the clockwise rotation (0/90/180/270) and the horizontal mirror a display matrix (a, b, c, d) states, as ffmpeg builds it: a rotation, then -display_hflip negating the first column.
func OrientationFromMatrix(a, b, c, d float64) (rotation int, hflip bool) {
	if math.Hypot(a, b) < 1e-3 {
		return 0, false // a first row of no length (a metadata track's 0 1e-5 0 0): not a rotation, ignored as ffmpeg ignores it
	}
	if a*d-b*c < 0 { // a mirror: take it off the first column to read the rotation left
		a, hflip = -a, true
	}
	if a == 0 && b == 0 {
		return 0, hflip
	}
	deg := int(math.Round(math.Atan2(b, a) * 180 / math.Pi))
	return ((deg % 360) + 360) % 360, hflip
}

// OrientationMatrix returns the display matrix (a, b, c, d) of a clockwise rotation and an optional horizontal mirror, the one ffmpeg writes for them.
func OrientationMatrix(rotation int, hflip bool) (a, b, c, d float64) {
	rad := float64(rotation) * math.Pi / 180
	cos, sin := math.Round(math.Cos(rad)*1e9)/1e9, math.Round(math.Sin(rad)*1e9)/1e9
	a, b, c, d = cos, sin, -sin, cos
	if hflip {
		a, c = -a, -c
	}
	return a + 0, b + 0, c + 0, d + 0 // +0 turns -0 into 0
}

// OrientationFromProjection returns the clockwise rotation and the horizontal mirror a flat video's Matroska pose states, read as ffmpeg reads it: a yaw of 180 mirrors horizontally, a pitch of 180 vertically, and with one mirror the roll turns the other way; a 360 projection or another pose states none.
func OrientationFromProjection(typ *uint64, yaw, pitch, roll float64) (rotation int, hflip bool) {
	if typ != nil && *typ != 0 {
		return 0, false
	}
	h, v := math.Abs(yaw) == 180, math.Abs(pitch) == 180
	if (yaw != 0 && !h) || (pitch != 0 && !v) {
		return 0, false
	}
	theta := roll // counterclockwise
	if h != v {
		theta = -roll
	}
	a, b, c, d := OrientationMatrix(int(math.Round(-theta)), false)
	if h {
		a, c = -a, -c
	}
	if v {
		b, d = -b, -d
	}
	return OrientationFromMatrix(a, b, c, d)
}

// ProjectionPose returns the Matroska ProjectionPoseYaw and ProjectionPoseRoll that state a clockwise rotation and a horizontal mirror, as ffmpeg writes them; the roll folded into (-180, 180].
func ProjectionPose(rotation int, hflip bool) (yaw, roll float64) {
	if hflip {
		return 180, foldDegrees(rotation)
	}
	return 0, foldDegrees(-rotation)
}

func foldDegrees(deg int) float64 {
	r := ((deg % 360) + 360) % 360
	if r > 180 {
		r -= 360
	}
	return float64(r)
}
