package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildBlockOrderedSource writes a source laid out in single-track BLOCKS of
// blockSec seconds: all the video clusters of a block, then all its audio
// clusters (whose timecodes go back to the block start) - what a two-input
// remux with a large interleave delta produces. Cluster and payload sizes
// follow buildInterleavedSource's measured figures, so every payload sits
// under the reader's seek threshold.
func buildBlockOrderedSource(tb testing.TB, seconds, blockSec int) string {
	tb.Helper()
	const (
		fps        = 25
		gopSec     = 2
		clusterMs  = 2000
		keyBytes   = 80 << 10
		deltaBytes = 14 << 10
		audioBytes = 1792
		audioMs    = 32
		scale      = 1_000_000
	)
	sample := func(n int, seed byte) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = seed + byte(i)
		}
		return b
	}
	w, h := uint32(1920), uint32(800)
	sr := 48000.0
	ch := uint8(6)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "fre"},
	}
	path := filepath.Join(tb.TempDir(), "blockordered.mkv")
	f, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		tb.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, int64(seconds)*1000); err != nil {
		tb.Fatal(err)
	}
	totalMs, blockMs := int64(seconds)*1000, int64(blockSec)*1000
	for bs := int64(0); bs < totalMs; bs += blockMs {
		be := min(bs+blockMs, totalMs)
		for cs := bs; cs < be; cs += clusterMs { // the block's video clusters ...
			clusterPos := m.RelPos()
			var blks []mkv.Block
			for fr := cs * fps / 1000; fr*1000/fps < cs+clusterMs; fr++ {
				ms := fr * 1000 / fps
				if ms < cs {
					continue
				}
				key := fr%(gopSec*fps) == 0
				data := sample(deltaBytes, byte(fr))
				if key {
					data = sample(keyBytes, byte(fr))
					m.Cues = append(m.Cues, mkv.CuePoint{TimeMs: ms, Track: 1, ClusterPos: clusterPos})
				}
				blks = append(blks, mkv.Block{TrackNumber: 1, Timecode: ms, Keyframe: key, Data: data})
			}
			if err := writer.WriteCluster(m.W, cs, scale, blks); err != nil {
				tb.Fatal(err)
			}
		}
		for cs := bs; cs < be; cs += clusterMs { // ... then its audio clusters
			var blks []mkv.Block
			for ms := cs; ms < cs+clusterMs; ms += audioMs {
				blks = append(blks, mkv.Block{TrackNumber: 2, Timecode: ms, Keyframe: true, Data: sample(audioBytes, byte(ms))})
			}
			if err := writer.WriteCluster(m.W, cs, scale, blks); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := m.Finalize(); err != nil {
		tb.Fatal(err)
	}
	return path
}

// TestPlanHLSBlockOrderedSource: on a block-ordered source the on-demand plan
// serves exactly the full pass's bytes, and once a window has taught it where
// each track's blocks are, the next window is read track by track - a few
// megabytes - instead of traversing the rest of the video block.
func TestPlanHLSBlockOrderedSource(t *testing.T) {
	ctx := context.Background()
	src := buildBlockOrderedSource(t, 60, 20) // 3 blocks of 20 s, 6 s windows
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}

	// Byte identity with the full pass, video and audio, every segment.
	full := t.TempDir()
	if err := RemuxToHLS(ctx, src, full, Options{SegmentMs: 6000}); err != nil {
		t.Fatal(err)
	}
	tally := &readTally{}
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 6000, FS: countingFS(tally)})
	if err != nil {
		t.Fatal(err)
	}
	fts := plan.fts()
	var served int64
	for n := 0; n < plan.NumSegments(); n++ {
		for i := range fts {
			name := renditionSegment(fts, i, n)
			got, _, err := plan.Resource(ctx, name)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			want, err := os.ReadFile(filepath.Join(full, name))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s differs from the full pass (%d vs %d bytes)", name, len(got), len(want))
			}
			served += int64(len(got))
		}
	}
	// Playing the file through must not read it many times over: the linear
	// walk traversed the rest of the video block for EVERY window (about 6x
	// the file on this layout); the track-by-track walk pays one traversal per
	// block boundary and the reader's read-ahead per track.
	if ratio := float64(tally.bytes) / float64(st.Size()); ratio > 2.5 {
		t.Errorf("whole play read %.2fx the file (%d MB read, %d MB served, %d MB file)", ratio, tally.bytes>>20, served>>20, st.Size()>>20)
	}

	// Window 1, with window 0's walk having revealed every track's opening
	// block, is read as one short walk per track.
	if starts := plan.trackPosAt(1); !plan.scatteredWindow(1, starts) {
		t.Fatalf("window 1 must be recognised as scattered after window 0's walk: %+v", starts)
	}
	fresh, err := PlanHLS(ctx, src, Options{SegmentMs: 6000, FS: countingFS(tally)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Segment(ctx, 0); err != nil { // teaches window 1's positions
		t.Fatal(err)
	}
	*tally = readTally{}
	v, err := fresh.Segment(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := fresh.Resource(ctx, renditionSegment(fts, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	window := int64(len(v) + len(a))
	if tally.bytes > window+2*(256<<10)+(512<<10) {
		t.Errorf("window 1 read %d KB for %d KB served: still traversing the block", tally.bytes>>10, window>>10)
	}
}

// An interleaved source keeps the single linear walk: its tracks open within
// the same cluster or two, never far enough apart to be read separately
// (which would read the window twice).
func TestPlanHLSInterleavedStaysLinear(t *testing.T) {
	ctx := context.Background()
	src := buildInterleavedSource(t, 30)
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 6000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Segment(ctx, 0); err != nil {
		t.Fatal(err)
	}
	starts := plan.trackPosAt(1)
	for i, s := range starts {
		if !s.Valid() {
			t.Fatalf("track %d: window 1's opening block not learned", i)
		}
	}
	if plan.scatteredWindow(1, starts) {
		t.Errorf("an interleaved window must not be read track by track: %+v", starts)
	}
}

// masterBandwidth extracts the first EXT-X-STREAM-INF BANDWIDTH of a master.
func masterBandwidth(t *testing.T, master []byte) int64 {
	t.Helper()
	m := regexp.MustCompile(`#EXT-X-STREAM-INF:BANDWIDTH=(\d+)`).FindSubmatch(master)
	if m == nil {
		t.Fatalf("no BANDWIDTH in master:\n%s", master)
	}
	v, _ := strconv.ParseInt(string(m[1]), 10, 64)
	return v
}

// On a block-ordered source the plan's BANDWIDTH (estimated from the cue
// offsets) must not read a foreign block as a bitrate peak: it stays within
// the full pass's figure (measured from the real segments), not 12x above.
func TestPlanHLSBlockOrderedBandwidth(t *testing.T) {
	ctx := context.Background()
	// 120 s blocks: the span straddling a boundary swallows two minutes of
	// audio, about four times the median rate - past the trigger, so the
	// file is asked and the block confirmed. (Shorter blocks stay under the
	// trigger and over-declare by less than 3x, by design.)
	src := buildBlockOrderedSource(t, 240, 120)
	full := t.TempDir()
	if err := RemuxToHLS(ctx, src, full, Options{SegmentMs: 6000}); err != nil {
		t.Fatal(err)
	}
	ref := masterBandwidth(t, readFileBytes(t, filepath.Join(full, "master.m3u8")))
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 6000})
	if err != nil {
		t.Fatal(err)
	}
	got := masterBandwidth(t, plan.MasterPlaylist())
	if got > 2*ref || got < ref/2 {
		t.Errorf("plan BANDWIDTH %d vs full pass %d: the estimate swallowed a foreign block", got, ref)
	}
	st, _ := os.Stat(src)
	if raw := peakBandwidth(rawSpans(plan, st.Size())); got >= raw {
		t.Errorf("plan BANDWIDTH %d must be below the raw cue-offset peak %d (the bound was not wired into the master)", got, raw)
	}
	if !bytes.Contains(plan.MasterPlaylist(), []byte("BANDWIDTH=")) {
		t.Fatal("no bandwidth")
	}
	mpd, _, err := plan.Resource(ctx, "manifest.mpd")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(mpd, []byte(`bandwidth="`+strconv.FormatInt(got, 10)+`"`)) {
		t.Errorf("the MPD must carry the same estimate %d:\n%s", got, mpd)
	}
}

func readFileBytes(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rawSpans recomputes the unbounded cue-offset spans of a plan.
func rawSpans(p *HLSPlan, size int64) []segInfo {
	var segs []segInfo
	for k := 0; k < p.segCount; k++ {
		end := size
		if k+1 < p.segCount {
			end = p.offsets[k+1]
		}
		segs = append(segs, segInfo{durSec: p.durs[k], bytes: end - p.offsets[k]})
	}
	return segs
}

// boundSegmentSpans asks the file only about spans past three times the
// median rate, replaces only the ones the file confirms as a foreign block,
// and keeps every real peak whatever its size.
func TestBoundSegmentSpans(t *testing.T) {
	asked := map[int]bool{}
	yes := func(k int) bool { asked[k] = true; return true }
	no := func(k int) bool { asked[k] = true; return false }

	even := []segInfo{{6, 600}, {6, 660}, {6, 540}, {6, 1500}, {6, 600}} // 2.5x peak: never asked
	for i, s := range boundSegmentSpans(even, yes) {
		if s != even[i] {
			t.Errorf("span %d changed: %+v -> %+v", i, even[i], s)
		}
	}
	if len(asked) != 0 {
		t.Errorf("spans under 3x the median must not be checked: %v", asked)
	}

	peak := []segInfo{{6, 600}, {6, 660}, {6, 3300}, {6, 540}, {6, 600}} // 5.5x real peak, the file says no
	asked = map[int]bool{}
	for i, s := range boundSegmentSpans(peak, no) {
		if s != peak[i] {
			t.Errorf("a real peak must be kept: span %d %+v -> %+v", i, peak[i], s)
		}
	}
	if !asked[2] || len(asked) != 1 {
		t.Errorf("only the 5.5x span must be checked: %v", asked)
	}

	block := []segInfo{{6, 600}, {6, 660}, {6, 60000}, {6, 540}, {3, 300}} // a swallowed block, confirmed
	got := boundSegmentSpans(block, yes)
	// The block is bounded to the median rate (100 B/s x 6 s = 600) and the
	// 59400 bytes taken out - the other tracks' media - come back to every
	// segment at their average rate over the 27 s: 2200 B/s.
	want := []int64{600 + 2200*6, 660 + 2200*6, 600 + 2200*6, 540 + 2200*6, 300 + 2200*3}
	for i := range got {
		if got[i].bytes != want[i] {
			t.Errorf("span %d: got %d bytes, want %d", i, got[i].bytes, want[i])
		}
	}
	if short := boundSegmentSpans([]segInfo{{6, 600}, {6, 60000}}, yes); short[1].bytes != 60000 {
		t.Error("fewer than three segments: no statistics, nothing bounded")
	}
	if nilc := boundSegmentSpans(block, nil); nilc[2].bytes != 60000 {
		t.Error("no checker: nothing bounded")
	}
}

// spanHoldsForeignBlock reads the cluster headers of the block-ordered
// fixture: a span straddling a block boundary holds a cluster whose timestamp
// steps back (true); a span inside the video block does not (false).
func TestSpanHoldsForeignBlock(t *testing.T) {
	src := buildBlockOrderedSource(t, 60, 20)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 6000})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	raw := rawSpans(plan, st.Size())
	// 20 s blocks, 6 s segments: segments 3 and 6 straddle a boundary (their
	// span runs into the audio block); segment 0 does not.
	for _, c := range []struct {
		k    int
		want bool
	}{{0, false}, {1, false}, {3, true}, {6, true}} {
		end := st.Size()
		if c.k+1 < plan.segCount {
			end = plan.offsets[c.k+1]
		}
		if got := spanHoldsForeignBlock(f, plan.offsets[c.k], end); got != c.want {
			t.Errorf("segment %d (span %d bytes): foreign = %v, want %v", c.k, raw[c.k].bytes, got, c.want)
		}
	}
}
