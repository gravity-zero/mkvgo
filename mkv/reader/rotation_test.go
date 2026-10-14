package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestProjectionPoseRollRotation reads Video>Projection>ProjectionPoseRoll (degrees
// counterclockwise, as a muxer writes a display rotation) into the clockwise
// Track.Rotation, through the seekable and the streaming parser.
func TestProjectionPoseRollRotation(t *testing.T) {
	cases := []struct {
		name  string
		proj  []byte
		want  int
		hflip bool
	}{
		{"roll 90", masterElem(mkv.IDProjection, floatElem(mkv.IDProjectionPoseRoll, 90)), 270, false},
		{"roll -90", masterElem(mkv.IDProjection, floatElem(mkv.IDProjectionPoseRoll, -90)), 90, false},
		{"roll 180", masterElem(mkv.IDProjection, floatElem(mkv.IDProjectionPoseRoll, 180)), 180, false},
		{"roll -180", masterElem(mkv.IDProjection, floatElem(mkv.IDProjectionPoseRoll, -180)), 180, false},
		{"rectangular type, roll 90", masterElem(mkv.IDProjection,
			uintElem(mkv.IDProjectionType, 0, 1), floatElem(mkv.IDProjectionPoseRoll, 90)), 270, false},
		{"yaw 180 mirrors, the roll then turns clockwise", masterElem(mkv.IDProjection,
			floatElem(mkv.IDProjectionPoseYaw, 180), floatElem(mkv.IDProjectionPoseRoll, 90)), 90, true},
		{"pitch 180 mirrors vertically: a horizontal mirror turned 180", masterElem(mkv.IDProjection,
			floatElem(mkv.IDProjectionPosePitch, 180)), 180, true},
		{"yaw 90 is no flat orientation", masterElem(mkv.IDProjection,
			floatElem(mkv.IDProjectionPoseYaw, 90), floatElem(mkv.IDProjectionPoseRoll, 90)), 0, false},
		{"equirectangular roll orients the sphere", masterElem(mkv.IDProjection,
			uintElem(mkv.IDProjectionType, 1, 1), floatElem(mkv.IDProjectionPoseRoll, 90)), 0, false},
		{"no roll", masterElem(mkv.IDProjection, uintElem(mkv.IDProjectionType, 0, 1)), 0, false},
	}
	for _, c := range cases {
		te := trackEntry(
			uintElem(mkv.IDTrackNumber, 1, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
			strElem(mkv.IDCodecID, "V_MPEG2"),
			masterElem(mkv.IDVideo, uintElem(mkv.IDPixelWidth, 720, 2), uintElem(mkv.IDPixelHeight, 576, 2), c.proj),
		)
		if tr := readFirstTrack(t, buildMKV(te)); tr.Rotation != c.want || tr.HFlip != c.hflip {
			t.Errorf("%s (seekable): Rotation, HFlip = %d, %v, want %d, %v", c.name, tr.Rotation, tr.HFlip, c.want, c.hflip)
		}

		var seg bytes.Buffer
		seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
		seg.Write(masterElem(mkv.IDTracks, te))
		cont, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
		if err != nil {
			t.Fatalf("%s: ReadStream: %v", c.name, err)
		}
		if tr := cont.Tracks[0]; tr.Rotation != c.want || tr.HFlip != c.hflip {
			t.Errorf("%s (stream): Rotation, HFlip = %d, %v, want %d, %v", c.name, tr.Rotation, tr.HFlip, c.want, c.hflip)
		}
	}
}
