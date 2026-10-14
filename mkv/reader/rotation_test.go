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
		name string
		proj []byte
		want int
	}{
		{"roll 90", masterElem(mkv.IDProjection, floatElem(mkv.IDProjectionPoseRoll, 90)), 270},
		{"roll -90", masterElem(mkv.IDProjection, floatElem(mkv.IDProjectionPoseRoll, -90)), 90},
		{"roll 180", masterElem(mkv.IDProjection, floatElem(mkv.IDProjectionPoseRoll, 180)), 180},
		{"roll -180", masterElem(mkv.IDProjection, floatElem(mkv.IDProjectionPoseRoll, -180)), 180},
		{"rectangular type, roll 90", masterElem(mkv.IDProjection,
			uintElem(mkv.IDProjectionType, 0, 1), floatElem(mkv.IDProjectionPoseRoll, 90)), 270},
		{"yaw 180 is a flip, not a rotation", masterElem(mkv.IDProjection,
			floatElem(mkv.IDProjectionPoseYaw, 180), floatElem(mkv.IDProjectionPoseRoll, 90)), 0},
		{"equirectangular roll orients the sphere", masterElem(mkv.IDProjection,
			uintElem(mkv.IDProjectionType, 1, 1), floatElem(mkv.IDProjectionPoseRoll, 90)), 0},
		{"no roll", masterElem(mkv.IDProjection, uintElem(mkv.IDProjectionType, 0, 1)), 0},
	}
	for _, c := range cases {
		te := trackEntry(
			uintElem(mkv.IDTrackNumber, 1, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
			strElem(mkv.IDCodecID, "V_MPEG2"),
			masterElem(mkv.IDVideo, uintElem(mkv.IDPixelWidth, 720, 2), uintElem(mkv.IDPixelHeight, 576, 2), c.proj),
		)
		if got := readFirstTrack(t, buildMKV(te)).Rotation; got != c.want {
			t.Errorf("%s (seekable): Rotation = %d, want %d", c.name, got, c.want)
		}

		var seg bytes.Buffer
		seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
		seg.Write(masterElem(mkv.IDTracks, te))
		cont, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
		if err != nil {
			t.Fatalf("%s: ReadStream: %v", c.name, err)
		}
		if got := cont.Tracks[0].Rotation; got != c.want {
			t.Errorf("%s (stream): Rotation = %d, want %d", c.name, got, c.want)
		}
	}
}
