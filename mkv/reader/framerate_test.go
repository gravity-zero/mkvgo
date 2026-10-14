package reader

import (
	"bytes"
	"context"
	"io"
	"math"
	"os"
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

// atSource is a source that says nothing of what it holds and offers exact reads (io.ReaderAt), as a local file does.
type atSource struct {
	plainSource
	at int
}

func (s *atSource) ReadAt(p []byte, off int64) (int, error) { s.at++; return s.r.ReadAt(p, off) }

// TestFrameRateReadsOption: by default the estimate reads nothing the source
// does not hold; WithFrameRateReads lets it read each block header it needs,
// exactly, within the head window.
func TestFrameRateReadsOption(t *testing.T) {
	data := segmentMKV(infoElem(), videoNoDurationTracks(), gopClustersOf(60, 24, 6, 40<<10))
	for _, c := range []struct {
		opts []ReadOption
		want bool
	}{{nil, false}, {[]ReadOption{WithFrameRateReads()}, true}} {
		for name, read := range map[string]func(io.ReadSeeker, ...ReadOption) (*mkv.Container, error){
			"ReadMeta": func(r io.ReadSeeker, o ...ReadOption) (*mkv.Container, error) {
				return ReadMeta(context.Background(), r, "x.mkv", o...)
			},
			"Read": func(r io.ReadSeeker, o ...ReadOption) (*mkv.Container, error) {
				return Read(context.Background(), r, "x.mkv", o...)
			},
		} {
			src := &atSource{plainSource: plainSource{r: bytes.NewReader(data)}}
			got, err := read(src, c.opts...)
			if err != nil {
				t.Fatal(err)
			}
			est := got.Tracks[0].EstimatedFrameRate
			if (est != nil) != c.want || c.want && *est != 24 {
				t.Errorf("%s with %d options: EstimatedFrameRate %v, want it %v (24)", name, len(c.opts), est, c.want)
			}
			if !c.want && src.at != 0 {
				t.Errorf("%s by default: %d exact reads, want none", name, src.at)
			}
			if c.want && (src.at == 0 || src.at > 64) {
				t.Errorf("%s with the option: %d exact reads, want one per block header, at most 64", name, src.at)
			}
		}
	}
}

// TestCodecFrameRate: without DefaultDuration, the frame rate the codec header
// states (the SPS timing of an avcC or hvcC, an MPEG-2 frame_rate_code) comes
// first, read from CodecPrivate with no read at all.
func TestCodecFrameRate(t *testing.T) {
	src, err := os.ReadFile("../../internal/testdata/sample.mkv")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Read(context.Background(), bytes.NewReader(src), "sample.mkv")
	if err != nil {
		t.Fatal(err)
	}
	avc := c.Tracks[0]
	mpeg2 := mkv.Track{Type: mkv.VideoTrack, Codec: "V_MPEG2",
		CodecPrivate: []byte{0, 0, 1, 0xB3, 0x2D, 0x02, 0x40, 0x34, 0xFF, 0xFF, 0xE0, 0x18}} // 720x576, 4:3, frame_rate_code 4
	for name, c := range map[string]struct {
		tr   mkv.Track
		want float64
	}{"H.264 SPS timing": {avc, 25}, "MPEG-2 frame_rate_code": {mpeg2, 30000.0 / 1001}} {
		got, ok := codecFrameRate(&c.tr)
		if !ok || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: %v %v, want %v", name, got, ok, c.want)
		}
	}
}

// TestCodecFrameRateNeedsFixedFlag: an H.264 SPS timing without
// fixed_frame_rate_flag may be a clock (a 2000 Hz tick reads as 1000 frames
// per second): it states a frame rate only when it names a standard one.
func TestCodecFrameRateNeedsFixedFlag(t *testing.T) {
	for _, c := range []struct {
		units, scale, fixed uint32
		want                float64
	}{{1001, 48000, 1, 24000.0 / 1001}, {1001, 48000, 0, 24000.0 / 1001}, {1, 2000, 0, 0}, {1, 2000, 1, 1000}} {
		b := spsPrefix()
		b.u(0, 1) // aspect_ratio_info_present_flag
		b.u(0, 1) // overscan_info_present_flag
		b.u(0, 1) // video_signal_type_present_flag
		b.u(0, 1) // chroma_loc_info_present_flag
		b.u(1, 1) // timing_info_present_flag
		b.u(c.units, 32)
		b.u(c.scale, 32)
		b.u(c.fixed, 1) // fixed_frame_rate_flag
		b.u(0, 1)       // nal_hrd_parameters_present_flag
		b.u(0, 1)       // vcl_hrd_parameters_present_flag
		b.u(0, 1)       // pic_struct_present_flag
		b.u(0, 1)       // bitstream_restriction_flag
		tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: wrapAvcC(b)}
		got, ok := codecFrameRate(&tr)
		if c.want == 0 && ok {
			t.Errorf("%d/%d fixed %d: %v, want none", c.scale, c.units, c.fixed, got)
		}
		if c.want != 0 && (!ok || math.Abs(got-c.want) > 1e-9) {
			t.Errorf("%d/%d fixed %d: %v %v, want %v", c.scale, c.units, c.fixed, got, ok, c.want)
		}
	}
}
