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

// demuxfrag_mut_test.go targets demux.go and fragment.go survivors: the
// per-field presence boundaries in buildMKVTracks, the cluster-window
// arithmetic in writeMKV, the decode-time sort in mergeByDTS, the running-max
// in movieDurationMs, the mp4ID/duration tracking and edit-list presence in
// the fMP4 init segment, the moof trun data_offset arithmetic and the
// composition-offset (hasCTS) detection in fillFragTiming.

// --- demux.go: buildMKVTracks field presence -------------------------------

// TestBuildMKVTracksFieldPresenceBoundaries kills demux.go:106 (display
// width/height, both operands), :129 (frame rate), :135 (channels), :139
// (sample rate) and :151 (bitrate): each optional mkv.Track field must be set
// exactly when its MP4 source value is positive, and omitted (nil) when 0.
func TestBuildMKVTracksFieldPresenceBoundaries(t *testing.T) {
	build := func(it inTrack) mkv.Track {
		mv := &movie{tracks: []inTrack{it}}
		return buildMKVTracks(mv, false)[0]
	}

	t.Run("display-dims-both-positive", func(t *testing.T) {
		tr := build(inTrack{trackType: mkv.VideoTrack, width: 640, height: 480, displayWidth: 704, displayHeight: 480})
		if tr.DisplayWidth == nil || *tr.DisplayWidth != 704 {
			t.Errorf("DisplayWidth = %v, want 704", tr.DisplayWidth)
		}
		if tr.DisplayHeight == nil || *tr.DisplayHeight != 480 {
			t.Errorf("DisplayHeight = %v, want 480", tr.DisplayHeight)
		}
	})
	t.Run("display-width-zero-omits-both", func(t *testing.T) {
		tr := build(inTrack{trackType: mkv.VideoTrack, width: 640, height: 480, displayWidth: 0, displayHeight: 480})
		if tr.DisplayWidth != nil || tr.DisplayHeight != nil {
			t.Errorf("display dims must be omitted when displayWidth is 0, got %v/%v", tr.DisplayWidth, tr.DisplayHeight)
		}
	})
	t.Run("display-height-zero-omits-both", func(t *testing.T) {
		tr := build(inTrack{trackType: mkv.VideoTrack, width: 640, height: 480, displayWidth: 704, displayHeight: 0})
		if tr.DisplayWidth != nil || tr.DisplayHeight != nil {
			t.Errorf("display dims must be omitted when displayHeight is 0, got %v/%v", tr.DisplayWidth, tr.DisplayHeight)
		}
	})

	t.Run("frame-rate-positive", func(t *testing.T) {
		tr := build(inTrack{trackType: mkv.VideoTrack, frameRate: 30})
		if tr.FrameRate == nil || *tr.FrameRate != 30 {
			t.Errorf("FrameRate = %v, want 30", tr.FrameRate)
		}
	})
	t.Run("frame-rate-zero-no-samples-omitted", func(t *testing.T) {
		tr := build(inTrack{trackType: mkv.VideoTrack, frameRate: 0})
		if tr.FrameRate != nil {
			t.Errorf("FrameRate must be nil when unknown and no samples, got %v", *tr.FrameRate)
		}
	})

	t.Run("channels-positive", func(t *testing.T) {
		tr := build(inTrack{trackType: mkv.AudioTrack, channels: 2})
		if tr.Channels == nil || *tr.Channels != 2 {
			t.Errorf("Channels = %v, want 2", tr.Channels)
		}
	})
	t.Run("channels-zero-omitted", func(t *testing.T) {
		tr := build(inTrack{trackType: mkv.AudioTrack, channels: 0})
		if tr.Channels != nil {
			t.Errorf("Channels must be nil when 0, got %v", *tr.Channels)
		}
	})

	t.Run("sample-rate-positive", func(t *testing.T) {
		tr := build(inTrack{trackType: mkv.AudioTrack, sampleRate: 48000})
		if tr.SampleRate == nil || *tr.SampleRate != 48000 {
			t.Errorf("SampleRate = %v, want 48000", tr.SampleRate)
		}
	})
	t.Run("sample-rate-zero-omitted", func(t *testing.T) {
		tr := build(inTrack{trackType: mkv.AudioTrack, sampleRate: 0})
		if tr.SampleRate != nil {
			t.Errorf("SampleRate must be nil when 0, got %v", *tr.SampleRate)
		}
	})

	t.Run("bitrate-positive", func(t *testing.T) {
		tr := build(inTrack{trackType: mkv.AudioTrack, bitrate: 128000})
		if tr.Bitrate == nil || *tr.Bitrate != 128000 {
			t.Errorf("Bitrate = %v, want 128000", tr.Bitrate)
		}
	})
	t.Run("bitrate-zero-omitted", func(t *testing.T) {
		tr := build(inTrack{trackType: mkv.AudioTrack, bitrate: 0})
		if tr.Bitrate != nil {
			t.Errorf("Bitrate must be nil when 0, got %v", *tr.Bitrate)
		}
	})
}

// TestMovieDurationMsTracksMax kills demux.go:311 (CONDITIONALS_NEGATION on
// `end > max`): the movie duration must be the largest per-track end time,
// even when it comes from a later track.
func TestMovieDurationMsTracksMax(t *testing.T) {
	mv := &movie{tracks: []inTrack{
		{samples: []inSample{{ctsMs: 100}, {ctsMs: 500}}},
		{samples: []inSample{{ctsMs: 900}}},
	}}
	if got := movieDurationMs(mv); got != 900 {
		t.Errorf("movieDurationMs = %d, want 900 (the max across every track)", got)
	}
}

// TestMergeByDTSOrderingAndStability kills demux.go:284 (the dtsMs < dtsMs
// sort comparator): samples must come out strictly ordered by decode time,
// and equal-DTS samples must keep their original per-track order (stable).
func TestMergeByDTSOrderingAndStability(t *testing.T) {
	mv := &movie{tracks: []inTrack{
		{samples: []inSample{{dtsMs: 200}, {dtsMs: 0}, {dtsMs: 50}}}, // track 0
		{samples: []inSample{{dtsMs: 100}, {dtsMs: 50}}},             // track 1
	}}
	refs := mergeByDTS(mv)
	if len(refs) != 5 {
		t.Fatalf("got %d refs, want 5", len(refs))
	}
	dtsOf := func(r sampleRef) int64 { return mv.tracks[r.track].samples[r.idx].dtsMs }
	prev := int64(-1)
	for i, r := range refs {
		d := dtsOf(r)
		if d < prev {
			t.Fatalf("ref %d out of order: dts %d < previous %d", i, d, prev)
		}
		prev = d
	}
	// The two dts=50 samples (track0/idx2 and track1/idx1) must keep track
	// 0 before track 1 (stable sort on a tie).
	var tieOrder []int
	for _, r := range refs {
		if dtsOf(r) == 50 {
			tieOrder = append(tieOrder, r.track)
		}
	}
	if len(tieOrder) != 2 || tieOrder[0] != 0 || tieOrder[1] != 1 {
		t.Errorf("dts=50 tie order = %v, want [0 1] (stable, track 0 before track 1)", tieOrder)
	}
}

// --- demux.go: writeMKV cluster windowing -----------------------------------

// clusterCount counts raw EBML Cluster IDs in an MKV file's bytes.
func clusterCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(data, []byte{0x1F, 0x43, 0xB6, 0x75})
}

// roundTripClusterCount builds a video-only MKV with the two given
// presentation timestamps (ms), round-trips it through RemuxToMP4 ->
// RemuxFromMP4, and returns the resulting MKV's cluster count.
func roundTripClusterCount(t *testing.T, pts0, pts1 int64) int {
	t.Helper()
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
	}
	blocks := []genBlock{
		{track: 1, pts: pts0, key: true, data: []byte{1, 2, 3, 4}},
		{track: 1, pts: pts1, key: true, data: []byte{5, 6, 7, 8}},
	}
	src := buildMKV(t, tracks, blocks)
	mp4Path := filepath.Join(t.TempDir(), "mid.mp4")
	if err := RemuxToMP4(context.Background(), src, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	outMKV := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, outMKV); err != nil {
		t.Fatalf("RemuxFromMP4: %v", err)
	}
	return clusterCount(t, outMKV)
}

// TestWriteMKVClusterWindowBoundary kills demux.go:249 (CONDITIONALS_BOUNDARY
// on `>= clusterWindowMs` and the dtsMs-groupStart arithmetic): a base offset
// of 2000ms is used so an INVERT_NEGATIVES/ARITHMETIC_BASE mutant turning the
// subtraction into an addition produces a wildly different (huge) delta,
// distinguishable from the correct one.
func TestWriteMKVClusterWindowBoundary(t *testing.T) {
	t.Run("just-under-window-one-cluster", func(t *testing.T) {
		// delta = 2999-2000 = 999 < 1000 -> no flush -> 1 cluster.
		// A mutant computing 2999+2000=4999 >= 1000 would wrongly flush -> 2.
		if got := roundTripClusterCount(t, 2000, 2999); got != 1 {
			t.Errorf("cluster count = %d, want 1 (delta 999ms stays under the 1000ms window)", got)
		}
	})
	t.Run("at-window-boundary-two-clusters", func(t *testing.T) {
		// delta = 3000-2000 = 1000 >= 1000 -> flush -> 2 clusters.
		// A ">" boundary mutant (1000 > 1000 == false) would wrongly keep 1.
		if got := roundTripClusterCount(t, 2000, 3000); got != 2 {
			t.Errorf("cluster count = %d, want 2 (delta 1000ms hits the window boundary)", got)
		}
	})
}

// --- fragment.go: buildInitSegment / buildInitTrak --------------------------

// makeFragTrackForInit returns a minimal *fragTrack (text handler, one
// sample) wrapping makeOutTrackForMoov, for exercising buildInitSegment's
// max-mp4ID/max-duration tracking.
func makeFragTrackForInit(mp4ID uint32, presentMs int64) *fragTrack {
	ot := makeOutTrackForMoov(mp4ID, presentMs)
	return &fragTrack{outTrack: ot, timescale: movieTimescale, durMediaTS: presentMs, presentMs: presentMs}
}

// TestBuildInitSegmentMaxIDAndDuration kills fragment.go:72 (NEGATION on
// `mp4ID > maxID`) and :75 (BOUNDARY on `presentMs > totalMs`): the mvhd's
// next_track_ID and duration must reflect the largest values across every
// track, not just the first or the track carrying the other maximum.
func TestBuildInitSegmentMaxIDAndDuration(t *testing.T) {
	t1 := makeFragTrackForInit(1, 500)
	t2 := makeFragTrackForInit(3, 200) // largest mp4ID, but not the longest
	t3 := makeFragTrackForInit(2, 900) // longest duration, but not the largest mp4ID
	init := buildInitSegment([]*fragTrack{t1, t2, t3}, movieMeta{}, nil)

	top, err := iterBoxes(init)
	if err != nil {
		t.Fatal(err)
	}
	moov, ok := findMemBox(top, "moov")
	if !ok {
		t.Fatal("moov not found")
	}
	moovBoxes, err := iterBoxes(moov.payload)
	if err != nil {
		t.Fatal(err)
	}
	mvhd, ok := findMemBox(moovBoxes, "mvhd")
	if !ok {
		t.Fatal("mvhd not found")
	}
	dur := binary.BigEndian.Uint32(mvhd.payload[16:20])
	if dur != 900 {
		t.Errorf("mvhd duration = %d, want 900 (max presentMs across tracks)", dur)
	}
	nextID := binary.BigEndian.Uint32(mvhd.payload[96:100])
	if nextID != 4 {
		t.Errorf("mvhd next_track_ID = %d, want 4 (max mp4ID 3 + 1)", nextID)
	}
}

// TestBuildInitTrakEditListPresence kills fragment.go:141 (both operands of
// `ft.offsetMs > 0 || codecDelay > 0`): an edts box must appear when either
// value is positive, and only then.
func TestBuildInitTrakEditListPresence(t *testing.T) {
	hasEdts := func(t *testing.T, trak []byte) bool {
		t.Helper()
		top, err := iterBoxes(trak)
		if err != nil {
			t.Fatal(err)
		}
		if len(top) != 1 {
			t.Fatalf("got %d top-level boxes, want 1 (the trak)", len(top))
		}
		children, err := iterBoxes(top[0].payload)
		if err != nil {
			t.Fatal(err)
		}
		_, ok := findMemBox(children, "edts")
		return ok
	}
	makeTrak := func(offsetMs, codecDelay int64) []byte {
		ot := &outTrack{
			mp4ID: 1,
			mkv:   mkv.Track{ID: 1, Type: mkv.AudioTrack, Codec: "aac", CodecDelay: codecDelay},
			spec:  codecSpec{handler: "soun"},
		}
		ot.sampleEntry = make([]byte, 8)
		ot.samples.addDur(1, 0, 0, 20, true)
		ot.samples.addChunk(0, 1)
		ft := &fragTrack{outTrack: ot, timescale: movieTimescale, durMediaTS: 100, presentMs: 100, offsetMs: offsetMs}
		return buildInitTrak(ft, nil)
	}

	t.Run("offset-only", func(t *testing.T) {
		if !hasEdts(t, makeTrak(50, 0)) {
			t.Error("edts must be present when offsetMs > 0")
		}
	})
	t.Run("codec-delay-only", func(t *testing.T) {
		if !hasEdts(t, makeTrak(0, 1_000_000)) {
			t.Error("edts must be present when codecDelay > 0")
		}
	})
	t.Run("neither", func(t *testing.T) {
		if hasEdts(t, makeTrak(0, 0)) {
			t.Error("edts must be absent when both offsetMs and codecDelay are 0")
		}
	})
}

// --- fragment.go: buildMoof / fillFragTiming --------------------------------

// TestBuildMoofDataOffsetArithmetic kills fragment.go:184 (NEGATION on
// `moofSize > 0`) and :185 (ARITHMETIC_BASE on both `+` operators): each
// track's trun data_offset must equal moofSize + mdatHeaderLen + the sum of
// the preceding tracks' dataLen.
func TestBuildMoofDataOffsetArithmetic(t *testing.T) {
	segs := []trackSegment{
		{trackID: 1, samples: []fragSample{{durTS: 40, size: 100, sync: true}}, dataLen: 100},
		{trackID: 2, samples: []fragSample{{durTS: 20, size: 200, sync: true}}, dataLen: 200},
	}
	moof := buildMoof(1, segs)
	moofLen := int32(len(moof))

	top, err := iterBoxes(moof)
	if err != nil {
		t.Fatal(err)
	}
	mf, ok := findMemBox(top, "moof")
	if !ok {
		t.Fatal("moof not found")
	}
	children, err := iterBoxes(mf.payload)
	if err != nil {
		t.Fatal(err)
	}
	var trafs []memBox
	for _, c := range children {
		if c.typ == "traf" {
			trafs = append(trafs, c)
		}
	}
	if len(trafs) != 2 {
		t.Fatalf("got %d traf boxes, want 2", len(trafs))
	}

	dataOffset := func(traf memBox) int32 {
		trafChildren, err := iterBoxes(traf.payload)
		if err != nil {
			t.Fatal(err)
		}
		trun, ok := findMemBox(trafChildren, "trun")
		if !ok {
			t.Fatal("trun not found")
		}
		// trun (version 1) payload: version/flags(4) + sample_count(4) + data_offset(4).
		return int32(binary.BigEndian.Uint32(trun.payload[8:12]))
	}

	want0 := moofLen + mdatHeaderLen + 0
	want1 := moofLen + mdatHeaderLen + 100
	if got := dataOffset(trafs[0]); got != want0 {
		t.Errorf("track 1 data_offset = %d, want %d (moofSize + mdatHeaderLen + 0)", got, want0)
	}
	if got := dataOffset(trafs[1]); got != want1 {
		t.Errorf("track 2 data_offset = %d, want %d (moofSize + mdatHeaderLen + track1's dataLen)", got, want1)
	}
}

// TestFillFragTimingHasCTS kills fragment.go:351 (NEGATION on `off != 0`):
// hasCTS must stay false when every sample's composition offset is zero, and
// flip true as soon as one sample is reordered (B-frames).
func TestFillFragTimingHasCTS(t *testing.T) {
	t.Run("no-reordering-no-cts", func(t *testing.T) {
		samples := []fragSample{{ptsMs: 0}, {ptsMs: 40}, {ptsMs: 80}}
		_, hasCTS, _, _ := fillFragTiming(samples, 0, movieTimescale, 0)
		if hasCTS {
			t.Error("hasCTS must be false when decode order matches presentation order")
		}
	})
	t.Run("reordering-sets-cts", func(t *testing.T) {
		// Decode order 0,120,40,80 (B-frames): the 2nd decoded sample (pts 120)
		// is not the 2nd-smallest pts, so its composition offset is nonzero.
		samples := []fragSample{{ptsMs: 0}, {ptsMs: 120}, {ptsMs: 40}, {ptsMs: 80}}
		_, hasCTS, _, _ := fillFragTiming(samples, 0, movieTimescale, 0)
		if !hasCTS {
			t.Error("hasCTS must be true when a sample's decode order differs from its presentation order")
		}
	})
}
