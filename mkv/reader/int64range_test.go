package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestValuesPastInt64AreUnknown: an unsigned value past MaxInt64 was read into an int64 field as a negative one - a
// DefaultDuration of -9223372036854775803 ns, a negative CodecDelay, a cue at a negative time or offset. Such a value
// is left unknown, and a cue point that holds no usable time or Cluster position is dropped.
func TestValuesPastInt64AreUnknown(t *testing.T) {
	const huge = 1<<63 + 5
	var te bytes.Buffer
	te.Write(uintEl(mkv.IDTrackNumber, 1))
	te.Write(uintEl(mkv.IDTrackType, mkv.TrackTypeAudio))
	ebml.WriteElementHeader(&te, mkv.IDCodecID, 6)
	ebml.WriteString(&te, "A_OPUS")
	te.Write(uintElN(mkv.IDDefaultDuration, huge, 8))
	te.Write(uintElN(mkv.IDCodecDelay, huge, 8))
	te.Write(uintElN(mkv.IDSeekPreRoll, huge, 8))

	point := func(time uint64, positions ...[]byte) []byte {
		body := uintElN(mkv.IDCueTime, time, 8)
		for _, p := range positions {
			body = append(body, wrapEl(mkv.IDCueTrackPositions, p)...)
		}
		return wrapEl(mkv.IDCuePoint, body)
	}
	pos := func(cluster, rel uint64) []byte {
		return append(append(uintEl(mkv.IDCueTrack, 1), uintElN(mkv.IDCueClusterPos, cluster, 8)...), uintElN(mkv.IDCueRelativePos, rel, 8)...)
	}
	var cues []byte
	cues = append(cues, point(huge, pos(10, 0))...)    // a time past int64
	cues = append(cues, point(1000, pos(huge, 0))...)  // a Cluster position past int64
	cues = append(cues, point(2000)...)                // no position at all
	cues = append(cues, point(3000, pos(10, huge))...) // usable; its relative position is not

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)
	seg.Write(wrapEl(mkv.IDTracks, wrapEl(mkv.IDTrackEntry, te.Bytes())))
	seg.Write(wrapEl(mkv.IDCues, cues))
	seg.Write(realCluster())
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	c, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	if tr.DefaultDurationNs != 0 || tr.FrameRate != nil || tr.CodecDelay != 0 || tr.SeekPreRoll != 0 {
		t.Errorf("default duration %d, frame rate %v, codec delay %d, pre-roll %d; want all unknown", tr.DefaultDurationNs, tr.FrameRate, tr.CodecDelay, tr.SeekPreRoll)
	}
	if len(c.Cues) != 1 || c.Cues[0].TimeMs != 3000 || c.Cues[0].ClusterPos != 10 || c.Cues[0].RelativePos != 0 {
		t.Errorf("cues %+v; want the 3000 ms point alone, at Cluster 10 with no relative position", c.Cues)
	}
}
