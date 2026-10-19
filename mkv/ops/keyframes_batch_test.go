package ops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/poison"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
	"github.com/gravity-zero/mkvgo/mp4"
)

// Bitstream samples, length-prefixed where the codec is.
var (
	kfSPS = testAVCSPS()
	kfPPS = testAVCPPS()
	// H.264 slices: IDR; non-IDR with slice_type 7 (I); non-IDR with slice_type 0 (P).
	avcIDR   = lp(testAVCSlice(0x65, 7, 0, 0))
	avcIntra = lp(testAVCSlice(0x41, 7, 9, 37))
	avcP     = lp([]byte{0x41, 0xC0, 0x00, 0x22})
	// HEVC: prefix SEI then IDR_W_RADL / CRA / TRAIL_R.
	hevcIDR   = lp([]byte{0x4E, 0x01, 0x05}, testHEVCSlice(19, 0, []byte{0xAF}))
	hevcCRA   = lp(testHEVCSlice(21, 3, []byte{0xAF}))
	hevcTrail = lp([]byte{0x02, 0x01, 0xAF})
	// AV1: sequence header OBU, key frame OBU, inter frame OBU.
	av1Seq   = []byte{0x0A, 0x03, 0x00, 0x00, 0x00}
	av1Key   = []byte{0x32, 0x02, 0x10, 0x00}
	av1Inter = []byte{0x32, 0x02, 0x30, 0x00}
	// VP9: key frame and inter frame headers.
	vp9Key   = []byte{0x82, 0x49, 0x83, 0x42, 0x00}
	vp9Inter = []byte{0x86, 0x00, 0x00}
)

func lp(nals ...[]byte) []byte {
	var out []byte
	for _, n := range nals {
		out = append(out, byte(len(n)>>24), byte(len(n)>>16), byte(len(n)>>8), byte(len(n)))
		out = append(out, n...)
	}
	return out
}

func avcCRecord() []byte {
	c := []byte{1, 0x64, 0x00, 0x1F, 0xFF, 0xE1, 0, byte(len(kfSPS))}
	c = append(c, kfSPS...)
	c = append(c, 1, 0, byte(len(kfPPS)))
	return append(c, kfPPS...)
}

// kfFixture writes a file with one video track (ID 1) and an audio track
// (ID 2): clusters of `perCluster` frames 40 ms apart, the first of each a
// keyframe (frame(i) gives every frame's payload), cued by the writer.
// keyAt moves each cluster's keyframe to its keyAt-th frame (the frames
// ahead of it are non-key video). editCues, when set, rewrites the cue list.
type kfFixture struct {
	track      mkv.Track
	clusters   int
	perCluster int
	frame      func(i int) []byte
	keyAt      int
	editCues   func([]mkv.CuePoint) []mkv.CuePoint
	noCues     bool
}

func (fx kfFixture) write(t testing.TB) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "in.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mw := writer.NewMKVWriter(f)
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000}}
	fx.track.ID = 1
	fx.track.Type = mkv.VideoTrack
	sr, ch := 48000.0, uint8(2)
	opusHead := []byte{'O', 'p', 'u', 's', 'H', 'e', 'a', 'd', 1, 2, 0x38, 0x01, 0x80, 0xBB, 0, 0, 0, 0, 0}
	tracks := []mkv.Track{fx.track, {ID: 2, Type: mkv.AudioTrack, Codec: "opus", CodecPrivate: opusHead, SampleRate: &sr, Channels: &ch}}
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	total := int64(fx.clusters * fx.perCluster * 40)
	if err := mw.WriteMetadata(c, tracks, total); err != nil {
		t.Fatal(err)
	}
	i := 0
	for k := 0; k < fx.clusters; k++ {
		var blocks []mkv.Block
		ts := int64(i) * 40
		for j := 0; j < fx.perCluster; j++ {
			blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 40, Keyframe: j == fx.keyAt, Data: fx.frame(i)})
			blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: int64(i) * 40, Keyframe: true, Data: []byte{0xFC, 1, 2}})
			i++
		}
		if err := mw.WriteClusterWithCues(ts, 1_000_000, blocks); err != nil {
			t.Fatal(err)
		}
	}
	switch {
	case fx.noCues:
		mw.Cues = nil
	case fx.editCues != nil:
		mw.Cues = fx.editCues(mw.Cues)
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return src
}

func h264Fixture(frame func(i int) []byte) kfFixture {
	w, h := uint32(320), uint32(240)
	return kfFixture{
		track:    mkv.Track{Codec: "h264", CodecPrivate: avcCRecord(), Width: &w, Height: &h},
		clusters: 20, perCluster: 25, // a keyframe every second, 20 s
		frame: frame,
	}
}

// key: the H.264 frame at index i - IDR on cluster starts, P elsewhere.
func avcFrames(i int) []byte {
	if i%25 == 0 {
		return avcIDR
	}
	return avcP
}

func collect(t *testing.T, src string, sel KeyframeSelect, opts ...mkv.Options) ([]KeyframeBatchSample, KeyframeStats) {
	t.Helper()
	var got []KeyframeBatchSample
	st, err := ExtractKeyframes(context.Background(), src, sel, func(s *KeyframeBatchSample) error {
		cp := *s
		cp.Data = append([]byte(nil), s.Data...)
		cp.Payload = append([]byte(nil), s.Payload...)
		got = append(got, cp)
		return nil
	}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return got, st
}

func ptsOf(ss []KeyframeBatchSample) []int64 {
	out := make([]int64, len(ss))
	for i := range ss {
		out[i] = ss[i].PtsMs
	}
	return out
}

// EveryMs keeps the first cued keyframe, then the first one at least EveryMs
// after the last kept - ffmpeg's select on prev_selected_t, on the cues.
func TestExtractKeyframesEvery(t *testing.T) {
	src := h264Fixture(avcFrames).write(t)
	got, st := collect(t, src, KeyframeSelect{EveryMs: 3000})
	want := []int64{0, 3000, 6000, 9000, 12000, 15000, 18000}
	if !reflect.DeepEqual(ptsOf(got), want) || !reflect.DeepEqual(st.PtsMs, want) {
		t.Fatalf("pts = %v (stats %v), want %v", ptsOf(got), st.PtsMs, want)
	}
	if st.Samples != len(want) || st.Skipped != 0 || st.Cues != 20 || st.MedianCueGapMs != 1000 {
		t.Fatalf("stats = %+v", st)
	}
	wantData := append([]byte{0, 0, 0, 1}, kfSPS...)
	wantData = append(append(wantData, 0, 0, 0, 1), kfPPS...)
	wantData = append(append(wantData, 0, 0, 0, 1), avcIDR[4:]...)
	for i, s := range got {
		if s.Index != i || s.Access != AccessIDR || s.Codec != "h264" || s.Ext != ".h264" {
			t.Fatalf("sample %d = %+v", i, s)
		}
		if !bytes.Equal(s.Data, wantData) || !bytes.Equal(s.Payload, avcIDR) {
			t.Fatalf("sample %d data %x payload %x", i, s.Data, s.Payload)
		}
		if s.Size != int64(len(avcIDR)) || s.Offset <= 0 {
			t.Fatalf("sample %d offset/size %d/%d", i, s.Offset, s.Size)
		}
		if want := int64(i) * 3000; s.TargetMs != want {
			t.Fatalf("sample %d target %d, want %d", i, s.TargetMs, want)
		}
	}
	// The payload lies where Offset/Size say.
	raw, _ := os.ReadFile(src)
	if s := got[2]; !bytes.Equal(raw[s.Offset:s.Offset+s.Size], avcIDR) {
		t.Fatalf("Offset/Size do not locate the payload")
	}
	if st.BytesRead <= 0 || st.Reads <= 0 || st.MetaBytesRead <= 0 {
		t.Fatalf("I/O not counted: %+v", st)
	}
}

// Start/End bound the selection; the first slot is StartMs.
func TestExtractKeyframesWindow(t *testing.T) {
	src := h264Fixture(avcFrames).write(t)
	got, _ := collect(t, src, KeyframeSelect{EveryMs: 2500, StartMs: 4500, EndMs: 12000})
	if want := []int64{5000, 8000, 11000}; !reflect.DeepEqual(ptsOf(got), want) {
		t.Fatalf("pts = %v, want %v", ptsOf(got), want)
	}
	if got[0].TargetMs != 4500 {
		t.Fatalf("first target %d, want 4500", got[0].TargetMs)
	}
}

// Targets resolve to the cued keyframe at or before each, any order,
// duplicates collapsed, and each one is the sample ExtractKeyframeSample
// returns for the same time (the batch is not a different extractor).
func TestExtractKeyframesTargetsMatchSingle(t *testing.T) {
	src := h264Fixture(avcFrames).write(t)
	targets := []int64{7300, -50, 2999, 7000, 19999, 2500, 30000}
	got, st := collect(t, src, KeyframeSelect{TargetsMs: targets})
	if want := []int64{0, 2000, 7000, 19000}; !reflect.DeepEqual(ptsOf(got), want) {
		t.Fatalf("pts = %v, want %v", ptsOf(got), want)
	}
	if st.Skipped != 0 {
		t.Fatalf("skipped %d", st.Skipped)
	}
	for _, s := range got {
		single, err := ExtractKeyframeSample(context.Background(), src, s.PtsMs)
		if err != nil {
			t.Fatal(err)
		}
		if single.PtsMs != s.PtsMs || !bytes.Equal(single.Data, s.Data) || single.Ext != s.Ext {
			t.Fatalf("batch %dms differs from single extraction", s.PtsMs)
		}
	}
}

// A cue landing on a P slice flagged as a keyframe is skipped, counted, and the
// next cue stays candidate for the same slot; a non-IDR all-intra picture is
// emitted as AccessIntra.
func TestExtractKeyframesSkipsNonAccessPoints(t *testing.T) {
	src := h264Fixture(func(i int) []byte {
		switch i {
		case 75: // cluster at 3 s: flagged key, P slice
			return avcP
		case 100: // cluster at 4 s: open-GOP intra
			return avcIntra
		}
		return avcFrames(i)
	}).write(t)
	got, st := collect(t, src, KeyframeSelect{EveryMs: 3000})
	if want := []int64{0, 4000, 7000, 10000, 13000, 16000, 19000}; !reflect.DeepEqual(ptsOf(got), want) {
		t.Fatalf("pts = %v, want %v", ptsOf(got), want)
	}
	if st.Skipped != 1 || got[1].Access != AccessIntra || got[1].TargetMs != 3000 {
		t.Fatalf("skipped %d, sample 1 = %s target %d", st.Skipped, got[1].Access, got[1].TargetMs)
	}
	// Targets mode: the bad cue is skipped, not replaced.
	got, st = collect(t, src, KeyframeSelect{TargetsMs: []int64{3500}})
	if len(got) != 0 || st.Skipped != 1 {
		t.Fatalf("targets: %d samples, %d skipped", len(got), st.Skipped)
	}
}

// A keyframe deep in its cluster (CueRelativePosition past relSeekMin) is
// reached by seating the reader on the block itself: the non-key video ahead
// of it in the cluster is never read.
func TestExtractKeyframesSeeksToRelativePosition(t *testing.T) {
	bigP := lp(append([]byte{0x41, 0xC0}, make([]byte, 64<<10)...))
	fx := h264Fixture(func(i int) []byte {
		if i%25 == 12 {
			return avcIDR
		}
		return bigP
	})
	fx.keyAt = 12
	src := fx.write(t)
	want := []int64{480, 5480, 10480, 15480}
	got, st := collect(t, src, KeyframeSelect{EveryMs: 5000})
	if !reflect.DeepEqual(ptsOf(got), want) {
		t.Fatalf("pts = %v, want %v", ptsOf(got), want)
	}
	ahead := int64(len(want)) * 12 * int64(len(bigP))
	if st.BytesRead >= ahead/4 {
		t.Fatalf("read %d bytes, the video ahead of the keyframes is %d", st.BytesRead, ahead)
	}
	// Without relative positions the walk still finds them, from the cluster start.
	fx.editCues = func(cues []mkv.CuePoint) []mkv.CuePoint {
		for i := range cues {
			cues[i].RelativePos = 0
		}
		return cues
	}
	got, st = collect(t, fx.write(t), KeyframeSelect{EveryMs: 5000})
	if !reflect.DeepEqual(ptsOf(got), want) {
		t.Fatalf("no RelativePos: pts = %v", ptsOf(got))
	}
	if st.BytesRead < ahead/2 {
		t.Fatalf("no RelativePos: read only %d bytes - the test no longer measures the seek", st.BytesRead)
	}
}

// Two cues inside one cluster are read in one forward walk; a cue that names
// no keyframe in its cluster is skipped.
func TestExtractKeyframesCuesInsideOneCluster(t *testing.T) {
	fx := h264Fixture(func(i int) []byte {
		if i%25 == 0 || i%25 == 12 {
			return avcIDR
		}
		return avcP
	})
	fx.editCues = func(cues []mkv.CuePoint) []mkv.CuePoint {
		var out []mkv.CuePoint
		for _, c := range cues {
			out = append(out, c, mkv.CuePoint{TimeMs: c.TimeMs + 480, Track: 1, ClusterPos: c.ClusterPos})
		}
		// A stale cue: a time no block of its cluster has.
		out = append(out, mkv.CuePoint{TimeMs: 19990, Track: 1, ClusterPos: cues[len(cues)-1].ClusterPos})
		return out
	}
	src := fx.write(t)
	got, st := collect(t, src, KeyframeSelect{TargetsMs: []int64{480, 1000, 1480, 19995}})
	if want := []int64{480, 1000, 1480}; !reflect.DeepEqual(ptsOf(got), want) {
		t.Fatalf("pts = %v, want %v", ptsOf(got), want)
	}
	if st.Skipped != 1 {
		t.Fatalf("skipped %d, want the stale cue", st.Skipped)
	}
}

func TestExtractKeyframesCodecs(t *testing.T) {
	w, h := uint32(64), uint32(48)
	av1C := append([]byte{0x81, 0x00, 0x0C, 0x00}, av1Seq...)
	cases := []struct {
		name, codec, ext, access string
		priv                     []byte
		key, inter, bad          []byte
		wantPayload              []byte
	}{
		{"hevc-idr", "hevc", ".hevc", AccessIDR, nil, hevcIDR, hevcTrail, hevcTrail, hevcIDR},
		// The CRA's POC LSB is renumbered: 64 for the first keyframe.
		{"hevc-cra", "hevc", ".hevc", AccessCRA, nil, hevcCRA, hevcTrail, hevcTrail, lp(testHEVCSlice(21, 64, []byte{0xAF}))},
		{"vp9", "vp9", ".ivf", AccessKey, nil, vp9Key, vp9Inter, vp9Inter, vp9Key},
		// The TU carries no sequence header: the av1C one is prepended.
		{"av1", "av1", ".ivf", AccessKey, av1C, av1Key, av1Inter, av1Inter, append(append([]byte(nil), av1Seq...), av1Key...)},
		// The TU carries its own: kept as is.
		{"av1-inband", "av1", ".ivf", AccessKey, av1C, append(append([]byte(nil), av1Seq...), av1Key...), av1Inter, av1Inter, append(append([]byte(nil), av1Seq...), av1Key...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := kfFixture{
				track:    mkv.Track{Codec: tc.codec, CodecPrivate: tc.priv, Width: &w, Height: &h},
				clusters: 6, perCluster: 25,
				frame: func(i int) []byte {
					switch {
					case i == 50:
						return tc.bad // cluster at 2 s: flagged key, not one
					case i%25 == 0:
						return tc.key
					}
					return tc.inter
				},
			}
			if tc.codec == "hevc" {
				fx.track.CodecPrivate = testHVCC()
			}
			got, st := collect(t, fx.write(t), KeyframeSelect{EveryMs: 1000})
			if want := []int64{0, 1000, 3000, 4000, 5000}; !reflect.DeepEqual(ptsOf(got), want) {
				t.Fatalf("pts = %v, want %v", ptsOf(got), want)
			}
			if st.Skipped != 1 {
				t.Fatalf("skipped %d", st.Skipped)
			}
			s := got[0]
			if s.Access != tc.access || s.Ext != tc.ext || !bytes.Equal(s.Payload, tc.wantPayload) {
				t.Fatalf("sample = %s %s payload %x, want %s %s %x", s.Access, s.Ext, s.Payload, tc.access, tc.ext, tc.wantPayload)
			}
			if tc.ext == ".ivf" && (!bytes.HasPrefix(s.Data, []byte("DKIF")) || !bytes.HasSuffix(s.Data, tc.wantPayload)) {
				t.Fatalf("IVF = %x", s.Data)
			}
		})
	}
}

// The errors a caller falls back on are typed.
func TestExtractKeyframesErrors(t *testing.T) {
	ctx := context.Background()
	noop := func(*KeyframeBatchSample) error { return nil }

	fx := h264Fixture(avcFrames)
	fx.noCues = true
	if _, err := ExtractKeyframes(ctx, fx.write(t), KeyframeSelect{EveryMs: 1000}, noop); !errors.Is(err, ErrNoKeyframeIndex) {
		t.Fatalf("no cues: %v", err)
	}
	fx = h264Fixture(avcFrames)
	fx.track.Codec = "mpeg4"
	if _, err := ExtractKeyframes(ctx, fx.write(t), KeyframeSelect{EveryMs: 1000}, noop); !errors.Is(err, ErrUnsupportedCodec) {
		t.Fatalf("mpeg4: %v", err)
	}

	src := h264Fixture(avcFrames).write(t)
	stop := errors.New("stop")
	n := 0
	st, err := ExtractKeyframes(ctx, src, KeyframeSelect{EveryMs: 1000}, func(*KeyframeBatchSample) error {
		if n++; n == 3 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || st.Samples != 2 {
		t.Fatalf("fn error: %v after %d samples", err, st.Samples)
	}
	cctx, cancel := context.WithCancel(ctx)
	st, err = ExtractKeyframes(cctx, src, KeyframeSelect{EveryMs: 1000}, func(*KeyframeBatchSample) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || st.Samples != 1 {
		t.Fatalf("cancel: %v after %d samples", err, st.Samples)
	}
}

func TestExtractKeyframesNoVideo(t *testing.T) {
	src := filepath.Join(t.TempDir(), "a.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	mw := writer.NewMKVWriter(f)
	sr := 48000.0
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000}},
		[]mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "opus", SampleRate: &sr}}, 1000); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteClusterWithCues(0, 1_000_000, []mkv.Block{{TrackNumber: 1, Keyframe: true, Data: []byte{1}}}); err != nil {
		t.Fatal(err)
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := ExtractKeyframes(context.Background(), src, KeyframeSelect{EveryMs: 1000}, func(*KeyframeBatchSample) error { return nil }); !errors.Is(err, ErrNoVideo) {
		t.Fatalf("audio only: %v", err)
	}
}

// writeCounter records each Write call.
type writeCounter struct {
	bytes.Buffer
	writes int
}

func (w *writeCounter) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

// The stream is a Matroska file holding the video track alone, its metadata
// kept, one keyframe per cluster at its real time, written one Write per
// cluster.
func TestWriteKeyframeStream(t *testing.T) {
	fx := h264Fixture(avcFrames)
	tr, pr := uint16(16), uint16(9)
	fx.track.ColorTransfer, fx.track.ColorPrimaries = &tr, &pr
	fx.track.HDR = &mkv.HDRStaticMetadata{MaxCLL: 1000, MaxFALL: 400}
	fx.track.DolbyVision = &mkv.DolbyVision{VersionMajor: 1, Profile: 8, Level: 6, RPUPresent: true, BLPresent: true, BLSignalCompatID: 1}
	src := fx.write(t)

	var out writeCounter
	st, err := WriteKeyframeStream(context.Background(), src, KeyframeSelect{EveryMs: 4000}, &out)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{0, 4000, 8000, 12000, 16000}
	if !reflect.DeepEqual(st.PtsMs, want) {
		t.Fatalf("pts = %v", st.PtsMs)
	}
	if out.writes != len(want) {
		t.Fatalf("%d writes for %d clusters", out.writes, len(want))
	}

	c, err := reader.Read(context.Background(), bytes.NewReader(out.Bytes()), "stream")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tracks) != 1 {
		t.Fatalf("%d tracks", len(c.Tracks))
	}
	got := c.Tracks[0]
	if got.Codec != "h264" || !bytes.Equal(got.CodecPrivate, fx.track.CodecPrivate) || *got.Width != 320 ||
		got.ColorTransfer == nil || *got.ColorTransfer != 16 || got.HDR == nil || got.HDR.MaxCLL != 1000 ||
		got.DolbyVision == nil || got.DolbyVision.Profile != 8 {
		t.Fatalf("track = %+v", got)
	}

	br, err := reader.NewBlockReader(bytes.NewReader(out.Bytes()), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	var pts []int64
	for {
		b, err := br.Next()
		if err != nil {
			break
		}
		if !b.Keyframe || !bytes.Equal(b.Data, avcIDR) {
			t.Fatalf("block %+v", b)
		}
		pts = append(pts, b.Timecode)
	}
	if !reflect.DeepEqual(pts, want) || br.ClusterCount() != int64(len(want)) {
		t.Fatalf("blocks %v in %d clusters", pts, br.ClusterCount())
	}
}

// A zlib-compressed source is written decompressed, its encoding not declared.
func TestWriteKeyframeStreamDecompresses(t *testing.T) {
	fx := h264Fixture(avcFrames)
	fx.track.HeaderStripping = []byte{0, 0, 0}
	fx.frame = func(i int) []byte { return avcFrames(i)[3:] }
	src := fx.write(t)
	var out bytes.Buffer
	if _, err := WriteKeyframeStream(context.Background(), src, KeyframeSelect{TargetsMs: []int64{0}}, &out); err != nil {
		t.Fatal(err)
	}
	c, err := reader.Read(context.Background(), bytes.NewReader(out.Bytes()), "stream")
	if err != nil {
		t.Fatal(err)
	}
	if c.Tracks[0].Compression != mkv.CompressionNone || len(c.Tracks[0].HeaderStripping) != 0 {
		t.Fatalf("stream still declares an encoding: %+v", c.Tracks[0])
	}
	if !bytes.Contains(out.Bytes(), avcIDR) {
		t.Fatalf("payload not restored")
	}
}

// Buffers are reused: the walk's allocations do not grow with the number of
// keyframes.
func TestExtractKeyframesAllocationsFlat(t *testing.T) {
	if poison.On {
		t.Skip("poisoning allocates a fresh buffer at each reuse")
	}
	allocs := func(clusters int) float64 {
		fx := h264Fixture(avcFrames)
		fx.clusters = clusters
		src := fx.write(t)
		return testing.AllocsPerRun(3, func() {
			if _, err := ExtractKeyframes(context.Background(), src, KeyframeSelect{EveryMs: 1}, func(*KeyframeBatchSample) error { return nil }); err != nil {
				t.Fatal(err)
			}
		})
	}
	small, large := allocs(20), allocs(200)
	// Per-keyframe cost: the stats' PtsMs growth and the block reader's
	// per-cluster bookkeeping, well under a few allocations per keyframe.
	if per := (large - small) / 180; per > 4 {
		t.Fatalf("%.1f allocations per keyframe (%v for 20, %v for 200)", per, small, large)
	}
}

// Regression: ExtractKeyframeSample used to take the cue at or before atMs of
// ANY track. A subtitle cue between two video keyframes named a cluster whose
// first video keyframe lies after atMs - the returned keyframe was later than
// asked, breaking "at or before".
func TestExtractKeyframeSampleIgnoresSubtitleCues(t *testing.T) {
	fx := h264Fixture(avcFrames)
	fx.editCues = func(cues []mkv.CuePoint) []mkv.CuePoint {
		// A subtitle cue at 2.5 s pointing at the cluster of 3 s.
		sub := mkv.CuePoint{TimeMs: 2500, Track: 2, ClusterPos: cues[3].ClusterPos}
		return append(cues[:3:3], append([]mkv.CuePoint{sub}, cues[3:]...)...)
	}
	src := fx.write(t)
	ks, err := ExtractKeyframeSample(context.Background(), src, 2700)
	if err != nil {
		t.Fatal(err)
	}
	if ks.PtsMs != 2000 {
		t.Fatalf("keyframe for 2700 ms = %d ms, want 2000", ks.PtsMs)
	}
}

// ExtractKeyframeSample gives an AV1 temporal unit without a sequence header
// the av1C one, or the IVF is undecodable alone.
func TestExtractKeyframeSampleAV1SequenceHeader(t *testing.T) {
	w, h := uint32(64), uint32(48)
	fx := kfFixture{
		track:    mkv.Track{Codec: "av1", CodecPrivate: append([]byte{0x81, 0x00, 0x0C, 0x00}, av1Seq...), Width: &w, Height: &h},
		clusters: 2, perCluster: 25,
		frame: func(i int) []byte {
			if i%25 == 0 {
				return av1Key
			}
			return av1Inter
		},
	}
	ks, err := ExtractKeyframeSample(context.Background(), fx.write(t), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if want := append(append([]byte(nil), av1Seq...), av1Key...); !bytes.HasSuffix(ks.Data, want) {
		t.Fatalf("IVF %x does not end with seq+frame %x", ks.Data, want)
	}
}

// An MP4 source goes through the same walk: its sync samples stand for the
// cues, each read with one exact range; the selection, the samples and the
// stream are the Matroska source's.
func TestExtractKeyframesMP4(t *testing.T) {
	src := h264Fixture(func(i int) []byte {
		if i == 100 {
			return avcIntra
		}
		return avcFrames(i)
	}).write(t)
	mp4Path := filepath.Join(t.TempDir(), "in.mp4")
	if err := mp4.RemuxToMP4(context.Background(), src, mp4Path); err != nil {
		t.Fatal(err)
	}
	want, _ := collect(t, src, KeyframeSelect{EveryMs: 3000})
	got, st := collect(t, mp4Path, KeyframeSelect{EveryMs: 3000})
	if !reflect.DeepEqual(ptsOf(got), ptsOf(want)) || st.Cues != 20 || st.Skipped != 0 {
		t.Fatalf("mp4 pts %v (stats %+v), mkv %v", ptsOf(got), st, ptsOf(want))
	}
	for i := range got {
		if !bytes.Equal(got[i].Payload, want[i].Payload) || got[i].Access != want[i].Access {
			t.Fatalf("sample %d differs: %s %x vs %s %x", i, got[i].Access, got[i].Payload, want[i].Access, want[i].Payload)
		}
	}
	if st.Reads != int64(len(got)) || st.MetaBytesRead <= 0 {
		t.Fatalf("expected one read per keyframe: %+v", st)
	}
	var out bytes.Buffer
	if _, err := WriteKeyframeStream(context.Background(), mp4Path, KeyframeSelect{EveryMs: 3000}, &out); err != nil {
		t.Fatal(err)
	}
	c, err := reader.Read(context.Background(), bytes.NewReader(out.Bytes()), "stream")
	if err != nil || len(c.Tracks) != 1 || c.Tracks[0].Codec != "h264" {
		t.Fatalf("stream: %v %+v", err, c)
	}
}
