package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// --- Test 1: exact counts on a multi-cluster video+audio fixture ---

func TestAnalyze_ExactCounts(t *testing.T) {
	dir := t.TempDir()
	video := videoTrack(1)
	audio := audioTrack(2)

	clusters := [][]mkv.Block{
		{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")}, {TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("a0")}},
		{{TrackNumber: 1, Timecode: 50, Keyframe: false, Data: []byte("v1")}, {TrackNumber: 2, Timecode: 50, Keyframe: true, Data: []byte("a1")}},
		{{TrackNumber: 1, Timecode: 100, Keyframe: true, Data: []byte("v2")}, {TrackNumber: 2, Timecode: 100, Keyframe: true, Data: []byte("a2")}},
		{{TrackNumber: 1, Timecode: 150, Keyframe: false, Data: []byte("v3")}, {TrackNumber: 2, Timecode: 150, Keyframe: true, Data: []byte("a3")}},
		{{TrackNumber: 1, Timecode: 200, Keyframe: false, Data: []byte("v4")}, {TrackNumber: 2, Timecode: 200, Keyframe: true, Data: []byte("a4")}},
	}
	path := buildMultiClusterMKV(t, dir, "exact.mkv", []mkv.Track{video, audio}, clusters, 250)

	report, err := Analyze(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if report.ClusterCount != 5 {
		t.Errorf("ClusterCount = %d, want 5", report.ClusterCount)
	}
	if len(report.Tracks) != 2 {
		t.Fatalf("Tracks = %d, want 2", len(report.Tracks))
	}
	byID := map[uint64]TrackStats{}
	for _, ts := range report.Tracks {
		byID[ts.TrackID] = ts
	}
	v, a := byID[1], byID[2]
	if v.Frames != 5 || v.Packets != 5 || v.Keyframes != 2 {
		t.Errorf("video: frames=%d packets=%d keyframes=%d, want 5/5/2", v.Frames, v.Packets, v.Keyframes)
	}
	if a.Frames != 5 || a.Packets != 5 || a.Keyframes != 5 {
		t.Errorf("audio: frames=%d packets=%d keyframes=%d, want 5/5/5", a.Frames, a.Packets, a.Keyframes)
	}
	if report.BlockCount != 10 {
		t.Errorf("BlockCount = %d, want 10", report.BlockCount)
	}
}

// --- Test 2: laced audio - Frames > Packets, exact ---

// vint returns the minimal-width EBML VINT encoding of v.
func vint(v uint64) []byte {
	var buf bytes.Buffer
	if _, err := ebml.WriteDataSize(&buf, int64(v)); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// aacDurNs is an AAC-LC frame at 48 kHz: 1024 samples = 21.333... ms.
const aacDurNs = 21_333_333

// writeRawLacedCluster appends one Cluster containing a single fixed-laced
// SimpleBlock (frameCount frames of frameSize bytes each, data[0] = frame
// index) directly to w - the writer package has no lacing support, real
// muxers never lace video, so this hand-built block is the only way to
// fixture one.
func writeRawLacedCluster(w *writer.MKVWriter, clusterTS int64, trackNum uint64, frameCount, frameSize int) error {
	var body bytes.Buffer
	if err := writer.WriteUintElement(&body, mkv.IDTimestamp, uint64(clusterTS)); err != nil {
		return err
	}

	trackBytes := vint(trackNum)
	payload := append([]byte{}, trackBytes...)
	payload = append(payload, 0x00, 0x00)                    // relTC = 0
	payload = append(payload, 0x80|0x04, byte(frameCount-1)) // keyframe | fixed lacing, lace count
	for i := 0; i < frameCount; i++ {
		frame := make([]byte, frameSize)
		frame[0] = byte(i)
		payload = append(payload, frame...)
	}
	if _, err := ebml.WriteElementHeader(&body, mkv.IDSimpleBlock, int64(len(payload))); err != nil {
		return err
	}
	if _, err := body.Write(payload); err != nil {
		return err
	}
	return writer.WriteMasterElement(w.W, mkv.IDCluster, body.Bytes())
}

func buildLacedAudioMKV(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	track := audioTrack(1)
	track.DefaultDurationNs = aacDurNs
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test"}}
	if err := mw.WriteMetadata(c, []mkv.Track{track}, 1000); err != nil {
		t.Fatal(err)
	}
	if err := writeRawLacedCluster(mw, 200, 1, 8, 2); err != nil {
		t.Fatal(err)
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAnalyze_LacedAudio_FramesExceedPackets(t *testing.T) {
	dir := t.TempDir()
	path := buildLacedAudioMKV(t, dir, "laced.mkv")

	report, err := Analyze(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Tracks) != 1 {
		t.Fatalf("Tracks = %d, want 1", len(report.Tracks))
	}
	ts := report.Tracks[0]
	if ts.Frames != 8 {
		t.Errorf("Frames = %d, want 8", ts.Frames)
	}
	if ts.Packets != 1 {
		t.Errorf("Packets = %d, want 1 (one stored laced block)", ts.Packets)
	}
	if ts.Frames <= ts.Packets {
		t.Errorf("Frames (%d) must exceed Packets (%d) for a laced track", ts.Frames, ts.Packets)
	}
}

// --- Test 3: GOP stats from known keyframe spacing ---

func TestAnalyze_GopStats(t *testing.T) {
	dir := t.TempDir()
	video := videoTrack(1)

	// Keyframes at 0, 100, 266: closed GOPs are 3 frames (0,33,66) and 5
	// frames (100,133,166,200,233); the trailing GOP from 266 has no
	// following keyframe, so it is never closed/counted.
	times := []struct {
		ms int64
		kf bool
	}{
		{0, true}, {33, false}, {66, false},
		{100, true}, {133, false}, {166, false}, {200, false}, {233, false},
		{266, true}, {299, false},
	}
	var blocks []mkv.Block
	for _, e := range times {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: e.ms, Keyframe: e.kf, Data: []byte{0x01}})
	}
	path := buildMultiClusterMKV(t, dir, "gop.mkv", []mkv.Track{video}, [][]mkv.Block{blocks}, 350)

	report, err := Analyze(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ts := report.Tracks[0]
	if ts.MinGopFrames != 3 {
		t.Errorf("MinGopFrames = %d, want 3", ts.MinGopFrames)
	}
	if ts.MaxGopFrames != 5 {
		t.Errorf("MaxGopFrames = %d, want 5", ts.MaxGopFrames)
	}
	if ts.AvgGopFrames != 4 {
		t.Errorf("AvgGopFrames = %v, want 4", ts.AvgGopFrames)
	}
	if ts.KeyframeEveryMsAvg != 133 {
		t.Errorf("KeyframeEveryMsAvg = %d, want 133 ((100+166)/2)", ts.KeyframeEveryMsAvg)
	}
	if ts.MaxKeyframeGapMs != 166 || ts.MaxKeyframeGapAtMs != 100 {
		t.Errorf("MaxKeyframeGapMs = %d at %d, want the 166ms span opening at the keyframe at 100ms", ts.MaxKeyframeGapMs, ts.MaxKeyframeGapAtMs)
	}
}

// --- Test 4: bitrate - exact average and the densest 1s window ---

func TestAnalyze_Bitrate(t *testing.T) {
	dir := t.TempDir()
	audio := audioTrack(1)

	// A dense early burst (0,100,200) followed by an isolated burst
	// (1500,1600): the peak window must track the densest span, dropping the
	// early frames once the walk moves past them, not just the running total.
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: make([]byte, 500)},
		{TrackNumber: 1, Timecode: 100, Keyframe: true, Data: make([]byte, 500)},
		{TrackNumber: 1, Timecode: 200, Keyframe: true, Data: make([]byte, 500)},
		{TrackNumber: 1, Timecode: 1500, Keyframe: true, Data: make([]byte, 2000)},
		{TrackNumber: 1, Timecode: 1600, Keyframe: true, Data: make([]byte, 100)},
	}
	path := buildMultiClusterMKV(t, dir, "bitrate.mkv", []mkv.Track{audio}, [][]mkv.Block{blocks}, 1700)

	report, err := Analyze(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ts := report.Tracks[0]
	// Bytes=3600, DurationMs=1600 (last frame's timecode, no duration info):
	// 3600*8*1000/1600 = 18000.
	if ts.Bytes != 3600 {
		t.Errorf("Bytes = %d, want 3600", ts.Bytes)
	}
	if ts.DurationMs != 1600 {
		t.Errorf("DurationMs = %d, want 1600", ts.DurationMs)
	}
	if ts.AvgBitrateBps != 18000 {
		t.Errorf("AvgBitrateBps = %d, want 18000", ts.AvgBitrateBps)
	}
	// Densest window: {1500,1600} = 2100 bytes = 16800 bps.
	if ts.PeakBitrateBps != 16800 {
		t.Errorf("PeakBitrateBps = %d, want 16800", ts.PeakBitrateBps)
	}
}

// --- Test 5: duration reconciliation warning ---

func TestAnalyze_DurationMismatchWarning(t *testing.T) {
	dir := t.TempDir()
	audio := audioTrack(1)
	audio.DefaultDurationNs = 100_000_000 // 100ms/frame, so the true end is exact

	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("a0")},
		{TrackNumber: 1, Timecode: 100, Keyframe: true, Data: []byte("a1")},
	}
	// True duration = 100 + 100 = 200ms; declared 5000ms is off by 4800ms.
	path := buildMultiClusterMKV(t, dir, "mismatch.mkv", []mkv.Track{audio}, [][]mkv.Block{blocks}, 5000)

	report, err := Analyze(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if report.DeclaredDurationMs != 5000 {
		t.Errorf("DeclaredDurationMs = %d, want 5000", report.DeclaredDurationMs)
	}
	if report.DurationMs != 200 {
		t.Errorf("DurationMs = %d, want 200", report.DurationMs)
	}
	if !warningsContain(report.Warnings, "declared duration") {
		t.Errorf("Warnings = %v, want a declared-vs-true duration mismatch warning", report.Warnings)
	}
}

// --- Test 6: head-only proof - the walk never reads a large payload whole ---

// spyReadSeekCloser wraps a real file, adding up the bytes any Read() call
// actually returns, so a test can prove the walk fetched far less than the
// full payload from the source - the head-only guarantee.
type spyReadSeekCloser struct {
	*os.File
	total *int64
}

func (s *spyReadSeekCloser) Read(p []byte) (int, error) {
	n, err := s.File.Read(p)
	*s.total += int64(n)
	return n, err
}

func TestAnalyze_HeadOnly_NeverReadsFullPayload(t *testing.T) {
	dir := t.TempDir()
	video := videoTrack(1)

	const frameSize = 200_000 // far larger than the reader's read-ahead window
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: make([]byte, frameSize)},
		{TrackNumber: 1, Timecode: 100, Keyframe: false, Data: make([]byte, frameSize)},
		{TrackNumber: 1, Timecode: 200, Keyframe: false, Data: make([]byte, frameSize)},
	}
	path := buildMultiClusterMKV(t, dir, "headonly.mkv", []mkv.Track{video}, [][]mkv.Block{blocks}, 300)
	totalPayload := int64(len(blocks)) * frameSize

	var totalRead int64
	fs := &mkv.FS{
		Open: func(p string) (mkv.ReadSeekCloser, error) {
			f, err := os.Open(p)
			if err != nil {
				return nil, err
			}
			return &spyReadSeekCloser{File: f, total: &totalRead}, nil
		},
	}

	report, err := Analyze(context.Background(), path, mkv.Options{FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	if report.Tracks[0].Frames != 3 || report.Tracks[0].Bytes != totalPayload {
		t.Fatalf("stats wrong: %+v", report.Tracks[0])
	}
	if totalRead >= totalPayload/2 {
		t.Errorf("read %d bytes from source, want well under half of the %d-byte payload (head-only walk)", totalRead, totalPayload)
	}
}

// --- Test 7: many clusters - correctness without holding all blocks ---
//
// Structural: Analyze accumulates per-track counters and a bounded bitrate
// window only (see trackAcc), never a slice of every block, so this is a
// correctness check rather than an RSS measurement.

func TestAnalyze_ManyClusters(t *testing.T) {
	dir := t.TempDir()
	video := videoTrack(1)

	const n = 500
	var clusters [][]mkv.Block
	for i := 0; i < n; i++ {
		clusters = append(clusters, []mkv.Block{
			{TrackNumber: 1, Timecode: int64(i) * 10, Keyframe: i%10 == 0, Data: []byte{byte(i)}},
		})
	}
	path := buildMultiClusterMKV(t, dir, "many.mkv", []mkv.Track{video}, clusters, int64(n)*10)

	report, err := Analyze(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if report.ClusterCount != n {
		t.Errorf("ClusterCount = %d, want %d", report.ClusterCount, n)
	}
	if report.Tracks[0].Frames != n {
		t.Errorf("Frames = %d, want %d", report.Tracks[0].Frames, n)
	}
	if report.Tracks[0].Keyframes != n/10 {
		t.Errorf("Keyframes = %d, want %d", report.Tracks[0].Keyframes, n/10)
	}
}

// --- Test 8: zero-frame / missing-duration / non-monotonic warnings ---

func TestAnalyze_Warnings(t *testing.T) {
	dir := t.TempDir()
	silent := videoTrack(1)   // never appears in any cluster
	noDur := audioTrack(2)    // frames, but no BlockDuration/DefaultDuration
	backward := audioTrack(3) // second frame's timecode jumps far backwards

	blocks := []mkv.Block{
		{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("n0")},
		{TrackNumber: 2, Timecode: 50, Keyframe: true, Data: []byte("n1")},
		{TrackNumber: 3, Timecode: 5000, Keyframe: true, Data: []byte("b0")},
		{TrackNumber: 3, Timecode: 0, Keyframe: true, Data: []byte("b1")},
	}
	path := buildMultiClusterMKV(t, dir, "warn.mkv", []mkv.Track{silent, noDur, backward}, [][]mkv.Block{blocks}, 100)

	report, err := Analyze(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !warningsContain(report.Warnings, "track 1 (video): no frames found") {
		t.Errorf("Warnings = %v, want a zero-frame warning for track 1", report.Warnings)
	}
	if !warningsContain(report.Warnings, "track 2 (audio): no frame duration available") {
		t.Errorf("Warnings = %v, want a missing-duration warning for track 2", report.Warnings)
	}
	if !warningsContain(report.Warnings, "track 3 (audio): timecode jumped backwards") {
		t.Errorf("Warnings = %v, want a backward-timecode warning for track 3", report.Warnings)
	}
}

// --- Test 9: FS port (MemFS) ---

func TestAnalyze_MemFS(t *testing.T) {
	mem := mkv.NewMemFS()
	fs := mem.FS()

	w, err := fs.DoCreate("mem.mkv")
	if err != nil {
		t.Fatal(err)
	}
	mw := writer.NewMKVWriter(w)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test"}}
	if err := mw.WriteMetadata(c, []mkv.Track{videoTrack(1)}, 200); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteClusterWithCues(0, 1_000_000, []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v0")},
		{TrackNumber: 1, Timecode: 100, Keyframe: false, Data: []byte("v1")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := Analyze(context.Background(), "mem.mkv", mkv.Options{FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Tracks) != 1 || report.Tracks[0].Frames != 2 {
		t.Fatalf("unexpected report from MemFS: %+v", report)
	}
}

// --- Test 10: CFR - equal frame durations classify as constant ---

func TestAnalyze_CFR(t *testing.T) {
	dir := t.TempDir()
	video := videoTrack(1)

	var blocks []mkv.Block
	for i := int64(0); i < 10; i++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: i * 40, Keyframe: i == 0, Data: []byte{0x01}})
	}
	path := buildMultiClusterMKV(t, dir, "cfr.mkv", []mkv.Track{video}, [][]mkv.Block{blocks}, 400)

	report, err := Analyze(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ts := report.Tracks[0]
	if ts.FrameRateMode != "cfr" {
		t.Errorf("FrameRateMode = %q, want cfr", ts.FrameRateMode)
	}
	if ts.FrameDurationVarianceNs != 0 {
		t.Errorf("FrameDurationVarianceNs = %d, want 0", ts.FrameDurationVarianceNs)
	}
	if warningsContain(report.Warnings, "variable frame rate") {
		t.Errorf("Warnings = %v, want no VFR warning for a CFR track", report.Warnings)
	}
}

// --- Test 11: VFR - deliberately uneven block timecodes classify as variable ---

func TestAnalyze_VFR(t *testing.T) {
	dir := t.TempDir()
	video := videoTrack(1)

	times := []int64{0, 33, 100, 133, 300, 333}
	var blocks []mkv.Block
	for i, ms := range times {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: ms, Keyframe: i == 0, Data: []byte{0x01}})
	}
	path := buildMultiClusterMKV(t, dir, "vfr.mkv", []mkv.Track{video}, [][]mkv.Block{blocks}, 400)

	report, err := Analyze(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ts := report.Tracks[0]
	if ts.FrameRateMode != "vfr" {
		t.Errorf("FrameRateMode = %q, want vfr", ts.FrameRateMode)
	}
	if ts.FrameDurationVarianceNs <= 0 {
		t.Errorf("FrameDurationVarianceNs = %d, want > 0", ts.FrameDurationVarianceNs)
	}
	if !warningsContain(report.Warnings, "variable frame rate") {
		t.Errorf("Warnings = %v, want a VFR warning", report.Warnings)
	}
}

// --- Test 11b: CFR + one aberrant delta stays constant ---

// A ms-quantised 24000/1001 track alternates 41ms and 42ms deltas; one
// dropped-frame hole must not flip the whole title to vfr - the raw max-min
// spread is dominated by that single outlier, the verdict must not be.
func TestAnalyze_CFRWithIsolatedOutlier(t *testing.T) {
	dir := t.TempDir()
	video := videoTrack(1)

	var blocks []mkv.Block
	tc := int64(0)
	for i := 0; i < 300; i++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: i == 0, Data: []byte{0x01}})
		switch {
		case i == 150:
			tc += 500
		case i%2 == 0:
			tc += 41
		default:
			tc += 42
		}
	}
	path := buildMultiClusterMKV(t, dir, "cfrhole.mkv", []mkv.Track{video}, [][]mkv.Block{blocks}, tc)

	report, err := Analyze(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ts := report.Tracks[0]
	if ts.FrameRateMode != "cfr" {
		t.Errorf("FrameRateMode = %q, want cfr (a single outlier is a glitch, not vfr)", ts.FrameRateMode)
	}
	if want := int64(500-41) * 1_000_000; ts.FrameDurationVarianceNs != want {
		t.Errorf("FrameDurationVarianceNs = %d, want %d (spread stays diagnostic)", ts.FrameDurationVarianceNs, want)
	}
	if f := ts.FrameDurationOutlierFrac; f <= 0 || f > 0.01 {
		t.Errorf("FrameDurationOutlierFrac = %v, want in (0, 0.01]", f)
	}
	if warningsContain(report.Warnings, "variable frame rate") {
		t.Errorf("Warnings = %v, want no VFR warning", report.Warnings)
	}
}

// --- Test 11b2: B-frame decode-order storage does not read as vfr ---

// Matroska stores blocks in decode order carrying presentation timecodes: on
// B-frame content the stored-order deltas jump around (e.g. +125, -84, +41)
// even though every frame is presented on a perfectly constant cadence. The
// classifier must restore presentation order before measuring durations.
func TestAnalyze_CFRWithBFrameReordering(t *testing.T) {
	dir := t.TempDir()
	video := videoTrack(1)

	// Presentation times on a 41/42ms alternating cadence, stored per group
	// of 4 as [I, P, B, B] = [t0, t3, t1, t2] - the P frame the Bs reference
	// is decoded (stored) before them.
	var times []int64
	tc := int64(0)
	for i := 0; i < 200; i++ {
		times = append(times, tc)
		if i%2 == 0 {
			tc += 41
		} else {
			tc += 42
		}
	}
	var blocks []mkv.Block
	for g := 0; g+4 <= len(times); g += 4 {
		for _, i := range []int{0, 3, 1, 2} {
			blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: times[g+i], Keyframe: g == 0 && i == 0, Data: []byte{0x01}})
		}
	}
	path := buildMultiClusterMKV(t, dir, "bframes.mkv", []mkv.Track{video}, [][]mkv.Block{blocks}, tc)

	report, err := Analyze(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ts := report.Tracks[0]
	if !ts.Reordered {
		t.Errorf("Reordered = false, want true (fixture stores frames in decode order)")
	}
	if ts.FrameRateMode != "cfr" {
		t.Errorf("FrameRateMode = %q, want cfr (reordering jitter is not frame-duration variance)", ts.FrameRateMode)
	}
	if ts.FrameDurationVarianceNs != 1_000_000 {
		t.Errorf("FrameDurationVarianceNs = %d, want 1000000 (the 41/42ms quantisation alternation)", ts.FrameDurationVarianceNs)
	}
	if ts.FrameDurationOutlierFrac != 0 {
		t.Errorf("FrameDurationOutlierFrac = %v, want 0", ts.FrameDurationOutlierFrac)
	}
}

// --- Test 11c: a rare outlier fraction (below threshold, above the lone-glitch guard) stays constant ---

func TestAnalyze_CFRRareOutliersStayCFR(t *testing.T) {
	dir := t.TempDir()
	video := videoTrack(1)

	var blocks []mkv.Block
	tc := int64(0)
	for i := 0; i < 300; i++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: i == 0, Data: []byte{0x01}})
		switch {
		case i == 100 || i == 200:
			tc += 300
		case i%2 == 0:
			tc += 41
		default:
			tc += 42
		}
	}
	path := buildMultiClusterMKV(t, dir, "cfrrare.mkv", []mkv.Track{video}, [][]mkv.Block{blocks}, tc)

	report, err := Analyze(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ts := report.Tracks[0]
	if ts.FrameRateMode != "cfr" {
		t.Errorf("FrameRateMode = %q, want cfr (2 outliers out of 299 deltas is below the vfr threshold)", ts.FrameRateMode)
	}
}

// --- Test 11d: a significant outlier fraction classifies as variable ---

func TestAnalyze_VFRSignificantOutlierFraction(t *testing.T) {
	dir := t.TempDir()
	video := videoTrack(1)

	// Deltas alternate 33ms and 50ms: about half the deltas sit far from the
	// modal delta - genuinely variable, not a glitch.
	var blocks []mkv.Block
	tc := int64(0)
	for i := 0; i < 100; i++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: i == 0, Data: []byte{0x01}})
		if i%2 == 0 {
			tc += 33
		} else {
			tc += 50
		}
	}
	path := buildMultiClusterMKV(t, dir, "vfrfrac.mkv", []mkv.Track{video}, [][]mkv.Block{blocks}, tc)

	report, err := Analyze(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ts := report.Tracks[0]
	if ts.FrameRateMode != "vfr" {
		t.Errorf("FrameRateMode = %q, want vfr", ts.FrameRateMode)
	}
	if f := ts.FrameDurationOutlierFrac; f < 0.4 {
		t.Errorf("FrameDurationOutlierFrac = %v, want >= 0.4", f)
	}
	if !warningsContain(report.Warnings, "variable frame rate") {
		t.Errorf("Warnings = %v, want a VFR warning", report.Warnings)
	}
}

// --- Test 12: a single-frame track has no measurable frame rate mode ---

func TestAnalyze_FrameRateMode_SingleFrame(t *testing.T) {
	dir := t.TempDir()
	video := videoTrack(1)

	blocks := []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01}}}
	path := buildMultiClusterMKV(t, dir, "oneframe.mkv", []mkv.Track{video}, [][]mkv.Block{blocks}, 100)

	report, err := Analyze(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ts := report.Tracks[0]
	if ts.FrameRateMode != "" {
		t.Errorf("FrameRateMode = %q, want empty (fewer than 2 frames)", ts.FrameRateMode)
	}
	if ts.FrameDurationVarianceNs != 0 {
		t.Errorf("FrameDurationVarianceNs = %d, want 0", ts.FrameDurationVarianceNs)
	}
}

func warningsContain(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
