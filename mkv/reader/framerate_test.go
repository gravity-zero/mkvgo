package reader

import (
	"bytes"
	"context"
	"math"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// videoNoDurationTracks is one H.264 track without DefaultDuration, as a muxer
// writes it when it does not know the frame rate.
func videoNoDurationTracks() []byte {
	return masterElem(mkv.IDTracks, trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackUID, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_MPEG4/ISO/AVC"),
		masterElem(mkv.IDVideo, uintElem(mkv.IDPixelWidth, 1920, 2), uintElem(mkv.IDPixelHeight, 1080, 2)),
	))
}

// gopClusters stores n frames at fps, timestamps rounded to the millisecond, in
// decode order (I P B B P B B ...), 30 frames and 4 KiB of payload per block to a cluster.
func gopClusters(n int, fps float64) []byte { return gopClustersOf(n, fps, 30, 4<<10) }

// gopClustersOf is gopClusters with per frames and payload bytes to a cluster.
func gopClustersOf(n int, fps float64, per, payload int) []byte {
	pts := func(k int) int64 { return int64(math.Round(float64(k) * 1000 / fps)) }
	var order []int
	order = append(order, 0)
	for k := 3; k-3 < n-1; k += 3 {
		for _, f := range []int{k, k - 2, k - 1} {
			if f < n {
				order = append(order, f)
			}
		}
	}
	var out []byte
	for i := 0; i < len(order); i += per {
		group := order[i:min(i+per, len(order))]
		base := pts(group[0])
		for _, f := range group {
			base = min(base, pts(f))
		}
		kids := [][]byte{uintElem(mkv.IDTimestamp, uint64(base), 4)}
		for _, f := range group {
			rel := pts(f) - base
			block := append([]byte{0x81, byte(rel >> 8), byte(rel), 0}, make([]byte, payload)...)
			var b bytes.Buffer
			ebml.WriteElementHeader(&b, mkv.IDSimpleBlock, int64(len(block)))
			b.Write(block)
			kids = append(kids, b.Bytes())
		}
		out = append(out, masterElem(mkv.IDCluster, kids...)...)
	}
	return out
}

// TestEstimatedFrameRate: without DefaultDuration the frame rate is fitted to
// the block timestamps of the first clusters within the head window, both on
// the metadata and on the full read; FrameRate stays nil.
func TestEstimatedFrameRate(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want float64
	}{
		{"25 fps", segmentMKV(infoElem(), videoNoDurationTracks(), gopClusters(60, 25)), 25},
		{"23.976 fps", segmentMKV(infoElem(), videoNoDurationTracks(), gopClusters(60, 24000.0/1001)), 24000.0 / 1001},
		{"24 fps", segmentMKV(infoElem(), videoNoDurationTracks(), gopClusters(60, 24)), 24},
		{"first frames past the head window", segmentMKV(infoElem(), videoNoDurationTracks(), voidElem(600<<10), gopClusters(60, 25)), 0},
		{"frames larger than the read buffer, 6 to a cluster", segmentMKV(infoElem(), videoNoDurationTracks(), gopClustersOf(60, 24, 6, 6<<10)), 24},
		{"first frames off the 25 fps grid", segmentMKV(infoElem(), videoNoDurationTracks(), stampedCluster(0, 61, 101, 141, 160, 221, 261, 301, 341, 381, 421, 461, 501, 541, 581, 621, 661, 701, 741, 781)), 25},
		{"too few frames to tell 24 from 23.976", segmentMKV(infoElem(), videoNoDurationTracks(), gopClusters(4, 24)), 0},
	}
	for _, c := range cases {
		for name, read := range map[string]func() (*mkv.Container, error){
			"ReadMeta": func() (*mkv.Container, error) {
				return ReadMeta(context.Background(), bytes.NewReader(c.data), "x.mkv")
			},
			"Read": func() (*mkv.Container, error) { return Read(context.Background(), bytes.NewReader(c.data), "x.mkv") },
		} {
			got, err := read()
			if err != nil {
				t.Fatalf("%s %s: %v", c.name, name, err)
			}
			v := got.Tracks[0]
			if v.FrameRate != nil {
				t.Errorf("%s %s: FrameRate = %v, want nil (no DefaultDuration)", c.name, name, *v.FrameRate)
			}
			switch {
			case c.want == 0 && v.EstimatedFrameRate != nil:
				t.Errorf("%s %s: EstimatedFrameRate = %v, want none", c.name, name, *v.EstimatedFrameRate)
			case c.want != 0 && (v.EstimatedFrameRate == nil || math.Abs(*v.EstimatedFrameRate-c.want) > 1e-9):
				t.Errorf("%s %s: EstimatedFrameRate = %v, want %v", c.name, name, v.EstimatedFrameRate, c.want)
			}
		}
	}
}

// stampedCluster is one cluster of 1 KiB video blocks at the given millisecond timestamps.
func stampedCluster(stamps ...int64) []byte {
	kids := [][]byte{uintElem(mkv.IDTimestamp, 0, 4)}
	for _, ts := range stamps {
		block := append([]byte{0x81, byte(ts >> 8), byte(ts), 0}, make([]byte, 1<<10)...)
		var b bytes.Buffer
		ebml.WriteElementHeader(&b, mkv.IDSimpleBlock, int64(len(block)))
		b.Write(block)
		kids = append(kids, b.Bytes())
	}
	return masterElem(mkv.IDCluster, kids...)
}

// plainSource is a source that says nothing of what it holds, like a network reader without Free.
type plainSource struct {
	r *bytes.Reader
	n int64
}

func (s *plainSource) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.n += int64(n)
	return n, err
}
func (s *plainSource) Seek(o int64, w int) (int64, error) { return s.r.Seek(o, w) }

// TestEstimatedFrameRateReadsNothingMore: the estimate reads only what the
// source already holds; from one that does not say, a file without
// DefaultDuration costs the same bytes as the same file with it.
func TestEstimatedFrameRateReadsNothingMore(t *testing.T) {
	tracks := func(dd []byte) []byte {
		return masterElem(mkv.IDTracks, trackEntry(
			uintElem(mkv.IDTrackNumber, 1, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
			strElem(mkv.IDCodecID, "V_MPEG4/ISO/AVC"),
			dd,
			masterElem(mkv.IDVideo, uintElem(mkv.IDPixelWidth, 1920, 2), uintElem(mkv.IDPixelHeight, 1080, 2)),
		))
	}
	cost := func(dd []byte) int64 {
		src := &plainSource{r: bytes.NewReader(segmentMKV(infoElem(), tracks(dd), voidElem(4<<10), gopClustersOf(60, 25, 6, 6<<10)))}
		if _, err := ReadMeta(context.Background(), src, "x.mkv"); err != nil {
			t.Fatal(err)
		}
		return src.n
	}
	with, without := cost(uintElem(mkv.IDDefaultDuration, 40_000_000, 4)), cost(voidElem(6))
	if without != with {
		t.Errorf("without DefaultDuration the read cost %d bytes, with it %d: the estimate read more", without, with)
	}
}

// statsTags is the per-track statistics tag a muxer writes: NUMBER_OF_FRAMES and DURATION for track uid.
func statsTags(uid uint64, frames, duration string) []byte {
	st := func(name, value string) []byte {
		return masterElem(mkv.IDSimpleTag, strElem(mkv.IDTagName, name), strElem(mkv.IDTagString, value))
	}
	return masterElem(mkv.IDTags, masterElem(mkv.IDTag,
		masterElem(mkv.IDTargets, uintElem(mkv.IDTargetTypeValue, 50, 1), uintElem(mkv.IDTagTrackUID, uid, 8)),
		st("NUMBER_OF_FRAMES", frames), st("DURATION", duration)))
}

// TestFrameRateFromStatisticsTags: the track's NUMBER_OF_FRAMES and DURATION
// statistics give its frame rate, a standard one when the ratio is within
// 2e-4 of it, with no read beyond the tags; they win over the timestamps.
func TestFrameRateFromStatisticsTags(t *testing.T) {
	cases := []struct {
		frames, duration string
		uid              uint64
		want             float64
	}{
		{"63274", "00:43:59.053000000", 1, 24000.0 / 1001},
		{"196351", "02:16:29.518000000", 1, 24000.0 / 1001},
		{"1000", "00:00:40.000000000", 1, 25},
		{"1000", "00:00:41.000000000", 1, 1000.0 / 41},
		{"1000", "00:00:40.000000000", 99, 0}, // a tag of another track
	}
	for _, c := range cases {
		data := segmentMKV(infoElem(), videoNoDurationTracks(), voidElem(600<<10), gopClusters(4, 24), statsTags(c.uid, c.frames, c.duration))
		got, err := Read(context.Background(), bytes.NewReader(data), "x.mkv")
		if err != nil {
			t.Fatal(err)
		}
		v := got.Tracks[0]
		switch {
		case c.want == 0 && v.EstimatedFrameRate != nil:
			t.Errorf("%s frames in %s for track %d: EstimatedFrameRate = %v, want none", c.frames, c.duration, c.uid, *v.EstimatedFrameRate)
		case c.want != 0 && (v.EstimatedFrameRate == nil || math.Abs(*v.EstimatedFrameRate-c.want) > 1e-9):
			t.Errorf("%s frames in %s: EstimatedFrameRate = %v, want %v", c.frames, c.duration, v.EstimatedFrameRate, c.want)
		}
	}
}
