package mp4

// The incremental subtitle-cue cursor must be invisible from the outside:
// every windowed sub segment and every whole-track .vtt must stay
// byte-identical to the full pass, whatever order the requests come in, and
// the first hit must cost a bounded prefix read instead of a whole-file scan.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildSubCursorFixture writes a ~300 s source designed to stress the cue
// windows: cues crossing segment boundaries, duration-less cues resolving on
// a far-away successor, a long cue spanning 15 segments, a duration-less
// final cue, and a second sparse "forced-style" track with a single late cue.
func buildSubCursorFixture(t testing.TB) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"},
		{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre", Name: "forcé"},
	}
	frame := append([]byte{0x00, 0x00, 0x00, 0x01, 0x65}, make([]byte, 2<<10)...)
	var blks []mkv.Block
	for i := 0; i < 1500; i++ { // 200ms frames, keyframe every 1s, 300s total
		blks = append(blks, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 200,
			Keyframe: i%5 == 0, Data: frame})
	}
	subs := []mkv.Block{
		{TrackNumber: 2, Timecode: 500, Duration: 4000, Data: []byte("A chevauche seg0-2")},
		{TrackNumber: 2, Timecode: 5900, Duration: 300, Data: []byte("B dans seg2")},
		{TrackNumber: 2, Timecode: 7900, Data: []byte("C sans durée, fin = début de D")},
		{TrackNumber: 2, Timecode: 8400, Data: []byte("D sans durée, fin = début de E (loin)")},
		{TrackNumber: 2, Timecode: 20000, Duration: 30000, Data: []byte("E longue, 15 segments")},
		{TrackNumber: 2, Timecode: 250000, Duration: 1000, Data: []byte("F tardive")},
		{TrackNumber: 2, Timecode: 299500, Data: []byte("G finale sans durée")},
		{TrackNumber: 3, Timecode: 250000, Duration: 2000, Data: []byte("piste creuse, cue unique")},
	}
	for _, b := range subs {
		b.Keyframe = true
		blks = append(blks, b)
	}
	for i := 1; i < len(blks); i++ { // keep timecode order (insertion sort, small tail)
		for j := i; j > 0 && blks[j].Timecode < blks[j-1].Timecode; j-- {
			blks[j], blks[j-1] = blks[j-1], blks[j]
		}
	}

	path := filepath.Join(t.TempDir(), "in.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const scale = 1_000_000
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, 300_000); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := writeTestClusters(m, scale, blks); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := m.Finalize(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// subResourceNames lists every subtitle resource of the plan: the whole-track
// .vtt files and all windowed segments, for both renditions.
func subResourceNames(plan *HLSPlan) []string {
	var names []string
	for k := 1; k <= 2; k++ {
		names = append(names, fmt.Sprintf("sub%d.vtt", k))
		for n := 1; n <= plan.NumSegments(); n++ {
			names = append(names, fmt.Sprintf("sub%d_%05d.vtt", k, n))
		}
	}
	return names
}

// Whatever order the subtitle resources are requested in - sequential,
// reverse, middle-out - each response must be byte-identical to the file the
// full pass writes. This is the exactness contract the incremental cursor
// must uphold (boundary-crossing cues, duration-less cues resolved on far
// successors, sparse tracks).
func TestPlanHLSSubtitleWindowsExactAnyOrder(t *testing.T) {
	src := buildSubCursorFixture(t)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}

	orders := map[string]func([]string) []string{
		"sequential": func(s []string) []string { return s },
		"reverse": func(s []string) []string {
			out := make([]string, len(s))
			for i, v := range s {
				out[len(s)-1-i] = v
			}
			return out
		},
		"middle-out": func(s []string) []string {
			var out []string
			for i := len(s) / 2; i < len(s); i += 7 {
				out = append(out, s[i])
			}
			return append(out, s...) // then everything, duplicates exercise the cache
		},
	}
	for name, reorder := range orders {
		plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
		if err != nil {
			t.Fatal(err)
		}
		for _, res := range reorder(subResourceNames(plan)) {
			got, _, err := plan.Resource(context.Background(), res)
			if err != nil {
				t.Fatalf("[%s] Resource(%s): %v", name, res, err)
			}
			want, err := os.ReadFile(filepath.Join(dir, res))
			if err != nil {
				t.Fatalf("[%s] full pass did not write %s: %v", name, res, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("[%s] %s differs from the full pass:\n got: %q\nwant: %q", name, res, got, want)
			}
		}
	}
}

// The first windowed request must cost a bounded prefix read - the direct-
// play property. The exact bound is the segment end plus the relative-
// timecode margin (~33 s at the standard scale): on this 300 s fixture that
// is well under a third of the file, where the previous whole-track scan
// read all of it.
func TestPlanHLSSubtitleFirstSegmentBoundedIO(t *testing.T) {
	src := buildSubCursorFixture(t)
	var readBytes int64
	fs := &mkv.FS{Open: func(path string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return countingRSC{f: f, n: &readBytes}, nil
	}}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000, FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt64(&readBytes, 0) // count the first windowed request alone
	vtt, _, err := plan.Resource(context.Background(), "sub1_00001.vtt")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(vtt, []byte("A chevauche")) {
		t.Fatalf("first window content wrong:\n%s", vtt)
	}
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if limit := st.Size() / 3; readBytes > limit {
		t.Errorf("first windowed request read %d of %d bytes (%.0f%%) - must be a bounded prefix, not a whole-track scan",
			readBytes, st.Size(), 100*float64(readBytes)/float64(st.Size()))
	}
}

// Sequential playback must pay roughly ONE pass in total: the cursor resumes
// where the previous request stopped, it never rescans the prefix.
func TestPlanHLSSubtitleScanIncremental(t *testing.T) {
	src := buildSubCursorFixture(t)
	var readBytes int64
	fs := &mkv.FS{Open: func(path string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return countingRSC{f: f, n: &readBytes}, nil
	}}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000, FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt64(&readBytes, 0)
	for n := 1; n <= plan.NumSegments(); n++ {
		if _, _, err := plan.Resource(context.Background(), fmt.Sprintf("sub1_%05d.vtt", n)); err != nil {
			t.Fatalf("segment %d: %v", n, err)
		}
	}
	if _, _, err := plan.Resource(context.Background(), "sub1.vtt"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if limit := st.Size() + st.Size()/3; readBytes > limit {
		t.Errorf("serving all windows sequentially read %d bytes for a %d-byte file (%.1fx) - the cursor must not rescan the prefix",
			readBytes, st.Size(), float64(readBytes)/float64(st.Size()))
	}
}

// cancelAfterRSC cancels a context after a fixed number of reads - a
// deterministic mid-scan client disconnect.
type cancelAfterRSC struct {
	f      mkv.ReadSeekCloser
	reads  *int64
	after  int64
	cancel context.CancelFunc
	armed  *atomic.Bool
}

func (c cancelAfterRSC) Read(p []byte) (int, error) {
	n, err := c.f.Read(p)
	if c.armed.Load() && atomic.AddInt64(c.reads, 1) == c.after {
		c.cancel()
	}
	return n, err
}
func (c cancelAfterRSC) Seek(offset int64, whence int) (int64, error) {
	return c.f.Seek(offset, whence)
}
func (c cancelAfterRSC) Close() error { return c.f.Close() }

// A client vanishing mid-scan must leave the cursor consistent: the next
// live request serves output byte-identical to the full pass - committed
// progress is kept, nothing is duplicated, nothing is lost.
func TestPlanHLSSubtitleCancelMidScanStaysExact(t *testing.T) {
	src := buildSubCursorFixture(t)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reads int64
	var armed atomic.Bool
	fs := &mkv.FS{Open: func(path string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return cancelAfterRSC{f: f, reads: &reads, after: 4, cancel: cancel, armed: &armed}, nil
	}}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000, FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	_, _, _ = plan.Resource(ctx, "sub1.vtt") // cancelled mid-scan (or served, timing-dependent)

	for _, res := range []string{"sub1.vtt", "sub1_00005.vtt", "sub1_00011.vtt", "sub2.vtt"} {
		got, _, err := plan.Resource(context.Background(), res)
		if err != nil {
			t.Fatalf("%s after mid-scan cancel: %v", res, err)
		}
		want, err := os.ReadFile(filepath.Join(dir, res))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the full pass after a mid-scan cancel:\n got: %q\nwant: %q", res, got, want)
		}
	}
}

// buildSeekFixture writes a ~900 s source whose subtitle blocks ALL carry
// explicit durations (the fast-seek precondition), including the trap: a cue
// starting 10 s BEFORE a window that a cold seek reaches without ever
// scanning the prefix - the backward margin must catch it.
func buildSeekFixture(t testing.TB) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"},
		{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre", Name: "forcé"},
	}
	frame := append([]byte{0x00, 0x00, 0x00, 0x01, 0x65}, make([]byte, 2<<10)...)
	var blks []mkv.Block
	for i := 0; i < 4500; i++ { // 200ms frames, keyframe every 1s, 900s total
		blks = append(blks, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 200,
			Keyframe: i%5 == 0, Data: frame})
	}
	subs := []mkv.Block{
		{TrackNumber: 2, Timecode: 1000, Duration: 3000, Data: []byte("tête")},
		{TrackNumber: 2, Timecode: 55_000, Duration: 5000, Data: []byte("dans la sonde")},
		{TrackNumber: 2, Timecode: 380_000, Duration: 15_000, Data: []byte("X déborde dans la fenêtre seekée")},
		{TrackNumber: 2, Timecode: 391_000, Duration: 2000, Data: []byte("dans la fenêtre")},
		{TrackNumber: 2, Timecode: 470_000, Duration: 4000, Data: []byte("après")},
		{TrackNumber: 2, Timecode: 899_000, Duration: 900, Data: []byte("fin")},
		{TrackNumber: 3, Timecode: 500_000, Duration: 2000, Data: []byte("piste creuse")},
	}
	for _, b := range subs {
		b.Keyframe = true
		blks = append(blks, b)
	}
	for i := 1; i < len(blks); i++ {
		for j := i; j > 0 && blks[j].Timecode < blks[j-1].Timecode; j-- {
			blks[j], blks[j-1] = blks[j-1], blks[j]
		}
	}

	path := filepath.Join(t.TempDir(), "in.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const scale = 1_000_000
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, 900_000); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := writeTestClusters(m, scale, blks); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := m.Finalize(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// segNameFor returns the 1-based subN_%05d.vtt name of the segment holding ms.
func segNameFor(plan *HLSPlan, track int, ms int64) string {
	n := 0
	for k := range plan.bounds {
		if plan.bounds[k] <= ms {
			n = k
		}
	}
	return fmt.Sprintf("sub%d_%05d.vtt", track, n+1)
}

// A cold seek to the middle of the presentation must serve the exact window
// - including the trap cue that started before the window - and every later
// request (early segments, other cold jumps, whole track, sparse track) must
// stay byte-identical to the full pass on the same plan instance.
func TestPlanHLSSubtitleColdSeekExact(t *testing.T) {
	src := buildSeekFixture(t)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}

	// Cold jump first: the window at 390s must include the cue started at 380s.
	trap := segNameFor(plan, 1, 390_000)
	got, _, err := plan.Resource(context.Background(), trap)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("X déborde")) {
		t.Fatalf("cold-seeked window %s misses the cue that started before it:\n%s", trap, got)
	}

	// Adversarial order on the same plan: more cold jumps, then the head,
	// then everything, whole tracks included.
	order := []string{
		trap,
		segNameFor(plan, 1, 392_000), // slide forward after the jump
		segNameFor(plan, 2, 500_000), // sparse track cold jump
		segNameFor(plan, 1, 898_000), // last-ish segment
		"sub1_00001.vtt",             // back to the head (prefix path)
		"sub1.vtt", "sub2.vtt",       // whole tracks
	}
	order = append(order, subResourceNames(plan)...)
	for _, res := range order {
		got, _, err := plan.Resource(context.Background(), res)
		if err != nil {
			t.Fatalf("Resource(%s): %v", res, err)
		}
		want, err := os.ReadFile(filepath.Join(dir, res))
		if err != nil {
			t.Fatalf("full pass did not write %s: %v", res, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the full pass:\n got: %q\nwant: %q", res, got, want)
		}
	}
}

// A cold seek must cost O(window), not O(position): the backward margin plus
// the window, never the whole prefix. And sliding forward from the seek point
// must reuse the island - near-zero additional I/O.
func TestPlanHLSSubtitleColdSeekBoundedIO(t *testing.T) {
	src := buildSeekFixture(t)
	var readBytes int64
	fs := &mkv.FS{Open: func(path string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return countingRSC{f: f, n: &readBytes}, nil
	}}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000, FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}

	atomic.StoreInt64(&readBytes, 0)
	if _, _, err := plan.Resource(context.Background(), segNameFor(plan, 1, 500_000)); err != nil {
		t.Fatal(err)
	}
	if limit := st.Size() / 3; readBytes > limit {
		t.Errorf("cold seek to 500s read %d of %d bytes (%.0f%%) - must cost O(window), not the whole prefix",
			readBytes, st.Size(), 100*float64(readBytes)/float64(st.Size()))
	}

	atomic.StoreInt64(&readBytes, 0)
	if _, _, err := plan.Resource(context.Background(), segNameFor(plan, 1, 502_000)); err != nil {
		t.Fatal(err)
	}
	if limit := st.Size() / 20; readBytes > limit {
		t.Errorf("sliding to the next segment read %d bytes (%.1f%% of the file) - the island must resume, not re-scan the lookback",
			readBytes, 100*float64(readBytes)/float64(st.Size()))
	}

	// A SECOND far jump must re-seek a fresh island, not drag the existing
	// one across the gap (500s → 850s would otherwise scan the whole span).
	atomic.StoreInt64(&readBytes, 0)
	if _, _, err := plan.Resource(context.Background(), segNameFor(plan, 1, 850_000)); err != nil {
		t.Fatal(err)
	}
	if limit := st.Size() / 3; readBytes > limit {
		t.Errorf("second far jump read %d of %d bytes (%.0f%%) - the island must be re-seeked, not extended across the gap",
			readBytes, st.Size(), 100*float64(readBytes)/float64(st.Size()))
	}
}

// Concurrent players on one plan: subtitle windows (prefix, island and
// whole-track paths racing on the same track), video segments and playlists
// served simultaneously must all succeed and stay byte-identical to the full
// pass. This is the locking contract of the per-track scan state - run under
// the race detector in CI.
func TestPlanHLSSubtitleConcurrentRequests(t *testing.T) {
	src := buildSeekFixture(t)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}

	reqs := []string{
		segNameFor(plan, 1, 390_000), // island
		segNameFor(plan, 1, 392_000), // island slide
		"sub1_00001.vtt",             // prefix
		"sub1_00002.vtt",
		segNameFor(plan, 2, 500_000), // second track island
		"sub1.vtt", "sub2.vtt",       // whole tracks (prefix to EOF)
		"master.m3u8", "sub1.m3u8",
		plan.SegmentName(0), plan.SegmentName(plan.NumSegments() - 1),
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(reqs)*4)
	for round := 0; round < 4; round++ {
		for _, res := range reqs {
			wg.Add(1)
			go func(res string) {
				defer wg.Done()
				got, _, err := plan.Resource(context.Background(), res)
				if err != nil {
					errs <- fmt.Errorf("%s: %w", res, err)
					return
				}
				switch res {
				case "master.m3u8": // BANDWIDTH is estimated on-demand
					return
				}
				want, err := os.ReadFile(filepath.Join(dir, res))
				if err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(got, want) {
					errs <- fmt.Errorf("%s differs from the full pass under concurrency", res)
				}
			}(res)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
