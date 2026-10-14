package mp4

import (
	"reflect"
	"testing"
)

// A packed rendition unpacks to the same samples and offsets: sizes, sync
// flags, decode times, durations and composition offsets, negative ones and
// far-apart offsets included; an empty rendition stays empty.
func TestPackTrackRoundTrip(t *testing.T) {
	seg := trackSegment{trackID: 2, baseDecodeTS: 90000, hasCTS: true, dataLen: 300000}
	var offs []int64
	dts, off := int64(90000), int64(5_000_000_000)
	for i := 0; i < 50; i++ {
		s := fragSample{size: uint32(1000 + 977*i), sync: i%25 == 0, dtsTS: dts, durTS: 3750 + int64(i%3), ctsTS: int32(7500 - 3750*(i%4))}
		seg.samples = append(seg.samples, s)
		offs = append(offs, off)
		dts += s.durTS
		off += int64(s.size) + int64(13*(i%5))
	}
	gotSeg, gotOffs := unpackTrack(packTrack(seg, offs))
	if !reflect.DeepEqual(gotSeg, seg) || !reflect.DeepEqual(gotOffs, offs) {
		t.Fatalf("round trip differs:\n%+v\n%+v", gotSeg, seg)
	}
	empty := trackSegment{trackID: 1, baseDecodeTS: 42}
	if got, o := unpackTrack(packTrack(empty, nil)); !reflect.DeepEqual(got, empty) || o != nil {
		t.Fatalf("empty: %+v %v", got, o)
	}
}
