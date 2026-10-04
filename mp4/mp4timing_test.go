package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// fieldPairSamples is the opening of an interlaced stream stored one field per
// sample: the first two fields carry the same presentation time, then the
// times move on in decode order - exactly the shape of a collapsed audio lace
// (a run of equal timecodes, then a larger one).
func fieldPairSamples() []fragSample {
	pts := []int64{0, 0, 133, 150, 33, 50, 67, 83, 100, 117, 200, 217, 167, 183}
	samples := make([]fragSample, len(pts))
	for i, p := range pts {
		samples[i] = fragSample{size: 100, ptsMs: p, blockPtsMs: p, sync: i == 0}
	}
	return samples
}

// Two video fields stamped with the same time are not a collapsed audio lace:
// a video track must never be put on the grid deriveGridTS recovers from one
// (67 ms a field here - a 59 s clip came out 238 s long).
func TestVideoIsNeverGridTimed(t *testing.T) {
	video := &outTrack{mkv: mkv.Track{Type: mkv.VideoTrack}}
	if got := audioGridTS(video, movieTimescale); got != noGridTS {
		t.Fatalf("audioGridTS(video) = %d, want noGridTS", got)
	}

	samples := fieldPairSamples()
	_, _, total, _ := fillFragTiming(samples, 0, movieTimescale, audioGridTS(video, movieTimescale))
	if total > 300 {
		t.Errorf("total duration = %d ms for fields spanning 217 ms: the video was grid-timed", total)
	}
	for i := 1; i < len(samples); i++ {
		if samples[i].durTS == 67 && samples[i-1].durTS == 67 {
			t.Fatalf("samples %d and %d both last 67 ms: the collapsed-lace stride was applied to video", i-1, i)
		}
	}

	window := fieldPairSamples()
	pt := &planTrack{ft: &fragTrack{outTrack: video, timescale: movieTimescale}, gridTS: noGridTS}
	timeSegmentWindow(window, pt, 233)
	var windowTotal int64
	for i := range window {
		windowTotal += window[i].durTS
	}
	if windowTotal > 300 {
		t.Errorf("on-demand window lasts %d ms for fields spanning 233 ms: the video was grid-timed", windowTotal)
	}
}

// A plan needs every segment head's length and none of its bytes:
// segmentFileLen must say what buildSegmentFile would have built.
func TestSegmentFileLenMatchesTheBuiltHead(t *testing.T) {
	for _, n := range []int{0, 1, 2, 150, 4000} {
		for _, cts := range []bool{false, true} {
			seg := trackSegment{trackID: 2, baseDecodeTS: 1 << 40, hasCTS: cts, dataLen: int64(n) * 5000,
				samples: make([]fragSample, n)}
			for i := range seg.samples {
				seg.samples[i] = fragSample{size: 5000, durTS: 1001, ctsTS: int32(i % 3), sync: i == 0}
			}
			if got, want := segmentFileLen(seg), int64(len(buildSegmentFile(7, seg))); got != want {
				t.Errorf("%d samples, hasCTS=%v: segmentFileLen = %d, built head = %d", n, cts, got, want)
			}
		}
	}
}

// nativeTicksMP4 builds a video-only MP4 and rewrites its video timing to
// 24000 ticks a second, 1001 a frame (23.976 frames a second: 41.708 ms, a
// duration the millisecond timeline cannot state).
func nativeTicksMP4(t *testing.T, frames int) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	for i := 0; i < frames; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%24 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	mkvSrc := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
	}, gblocks)
	src := filepath.Join(t.TempDir(), "in.mp4")
	if err := RemuxToMP4(context.Background(), mkvSrc, src, Options{FastStart: true}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	mdhd := bytes.Index(data, []byte("mdhd"))
	stts := bytes.Index(data, []byte("stts"))
	if mdhd < 0 || stts < 0 || data[mdhd+4] != 0 ||
		binary.BigEndian.Uint32(data[mdhd+16:]) != movieTimescale ||
		binary.BigEndian.Uint32(data[stts+8:]) != 1 || binary.BigEndian.Uint32(data[stts+16:]) != 40 {
		t.Fatal("the fixture's video mdhd/stts is not the millisecond single-entry table this test rewrites")
	}
	binary.BigEndian.PutUint32(data[mdhd+16:], 24000)
	binary.BigEndian.PutUint32(data[mdhd+20:], uint32(frames)*1001)
	binary.BigEndian.PutUint32(data[stts+16:], 1001)
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return src
}

// An MP4 source states its video frames in its own ticks. The fragments must
// keep them - a constant 1001 at 24000 - instead of the 41 and 42 ms the
// millisecond timeline rounds them to, and the plan must equal the full pass.
func TestMP4SourceVideoKeepsItsNativeTicks(t *testing.T) {
	src := nativeTicksMP4(t, 240)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	ft := plan.tracks[0].ft
	if ft.timescale != 24000 {
		t.Fatalf("video timescale = %d, want the source's 24000", ft.timescale)
	}
	durs := map[int64]int{}
	for k := 0; k < plan.NumSegments(); k++ {
		samples, _ := plan.mp4tabs[0].window(k)
		for i := range samples {
			durs[samples[i].durTS]++
		}
	}
	if len(durs) != 1 || durs[1001] != 240 {
		t.Errorf("video durations = %v, want 240 frames of 1001 ticks", durs)
	}
	if ft.durMediaTS != 240*1001 {
		t.Errorf("media duration = %d ticks, want %d", ft.durMediaTS, 240*1001)
	}
	init, _, err := plan.Resource(context.Background(), "init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if at := bytes.Index(init, []byte("mdhd")); at < 0 || binary.BigEndian.Uint32(init[at+16:]) != 24000 {
		t.Errorf("the init segment's mdhd does not declare the 24000 timescale")
	}

	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	for _, name := range plan.Resources() {
		got, _, err := plan.Resource(context.Background(), name)
		if err != nil {
			t.Errorf("Resource(%q): %v", name, err)
			continue
		}
		want, ferr := os.ReadFile(filepath.Join(dir, name))
		if ferr != nil {
			t.Errorf("full pass did not write %s", name)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the full pass (%d vs %d bytes)", name, len(got), len(want))
		}
	}
}
