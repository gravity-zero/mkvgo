package mp4

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

// openPlan is what serves resources both whole and through a handle: HLSPlan, ConcatPlan.
type openPlan interface {
	Resources() []string
	Resource(ctx context.Context, name string) ([]byte, string, error)
	Open(ctx context.Context, name string) (*ResourceHandle, error)
}

// assertOpenMatchesResource opens every resource and checks the handle's size,
// its whole write, its middle third and a few sub-ranges against Resource's bytes.
func assertOpenMatchesResource(t *testing.T, plan openPlan) {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(3))
	for _, name := range plan.Resources() {
		want, ct, err := plan.Resource(ctx, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		h, err := plan.Open(ctx, name)
		if err != nil {
			t.Fatalf("Open(%s): %v", name, err)
		}
		if h.ContentType() != ct || h.Size() != int64(len(want)) {
			t.Errorf("%s: handle says %q %d bytes, Resource %q %d bytes", name, h.ContentType(), h.Size(), ct, len(want))
		}
		var got bytes.Buffer
		if m, err := h.WriteTo(ctx, &got); err != nil || m != int64(len(want)) {
			t.Fatalf("%s: WriteTo wrote %d, err %v", name, m, err)
		}
		if !bytes.Equal(got.Bytes(), want) {
			t.Errorf("%s: streamed bytes differ from Resource (%d vs %d)", name, got.Len(), len(want))
		}
		if third := int64(len(want)) / 3; third > 0 {
			var mid bytes.Buffer
			if m, err := h.WriteRange(ctx, &mid, third, third); err != nil || m != third || !bytes.Equal(mid.Bytes(), want[third:2*third]) {
				t.Errorf("%s: WriteRange(%d,%d) wrote %d, err %v, or differs", name, third, third, m, err)
			}
		}
		for i := 0; i < 3 && len(want) > 1; i++ {
			off := rng.Int63n(int64(len(want)))
			n := rng.Int63n(int64(len(want)) - off + 1)
			var part bytes.Buffer
			if _, err := h.WriteRange(ctx, &part, off, n); err != nil {
				t.Fatalf("%s: WriteRange(%d,%d): %v", name, off, n, err)
			}
			if !bytes.Equal(part.Bytes(), want[off:off+n]) {
				t.Errorf("%s: WriteRange(%d,%d) differs", name, off, n)
			}
		}
		h.Close()
	}
}

// Open streams every segment of the reordered, jittered and audio-ahead
// fixtures byte for byte like Resource, sub-ranges included.
func TestOpenStreamsIdenticalBytes(t *testing.T) {
	ctx := context.Background()
	for name, src := range map[string]string{
		"open-gop":    openGOPSource(t, 5, true, true),
		"grid-jitter": jitteredGridSource(t),
		"audio-ahead": func() string {
			w, h := uint32(320), uint32(240)
			sr, ch := 44100.0, uint8(2)
			var blocks []genBlock
			ai := 0
			for i := 0; i < 150; i++ {
				for ; int64(ai)*20 < int64(i)*40+60; ai++ {
					blocks = append(blocks, genBlock{track: 2, pts: int64(ai) * 20, key: true, data: []byte{0xAA, byte(ai)}})
				}
				blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: iframeTestFrame(i, i%25 == 0)})
			}
			return buildKeyframeClusteredMKV(t, []mkv.Track{
				{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
				{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
			}, blocks)
		}(),
	} {
		for _, kept := range []bool{true, false} { // served from the walk's bytes, then copied from the source
			t.Run(fmt.Sprintf("%s/kept=%v", name, kept), func(t *testing.T) {
				if !kept {
					copyFromSource(t)
				}
				segMs := int64(2000)
				if name == "open-gop" {
					segMs = openGOPMs
				}
				plan, err := PlanHLS(ctx, src, keptOptions(kept, Options{SegmentMs: segMs}))
				if err != nil {
					t.Fatal(err)
				}
				assertOpenMatchesResource(t, plan)
				st := plan.Stats()
				if st.TableBuilds == 0 || st.StreamFallbacks != 0 || (kept && st.KeptSegments == 0) || (!kept && st.StreamedSegments == 0) {
					t.Errorf("stats: %+v", st)
				}
			})
		}
	}
}

// An MP4 source streams every rendition's segments from the file, byte for
// byte like Resource: progressive, fast-start and fragmented.
func TestOpenStreamsMP4Sources(t *testing.T) {
	ctx := context.Background()
	mkvSrc, _ := buildLacedFixture(t)
	frag, _ := makeFragmentedMP4(t)
	dir := t.TempDir()
	srcs := map[string]string{"fragmented": filepath.Join(dir, "frag.mp4")}
	if err := os.WriteFile(srcs["fragmented"], frag, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, fast := range map[string]bool{"progressive": false, "faststart": true} {
		srcs[name] = filepath.Join(dir, name+".mp4")
		if err := RemuxToMP4(ctx, mkvSrc, srcs[name], Options{FastStart: fast}); err != nil {
			t.Fatal(err)
		}
	}
	for name, src := range srcs {
		t.Run(name, func(t *testing.T) {
			plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
			if err != nil {
				t.Fatal(err)
			}
			assertOpenMatchesResource(t, plan)
			if st := plan.Stats(); st.StreamedSegments == 0 || st.StreamFallbacks != 0 {
				t.Errorf("stats: %+v", st)
			}
		})
	}
}

// takeWriter records what a writer was given by Write and what it took from the file through ReadFrom.
type takeWriter struct {
	bytes.Buffer
	fromFile int64
}

func (w *takeWriter) ReadFrom(r io.Reader) (int64, error) {
	n, err := w.Buffer.ReadFrom(r)
	w.fromFile += n
	return n, err
}

// Samples stored back to back, each shorter than the sendfile threshold, go
// to the writer as one run from the file, not through the buffer.
func TestOpenCopiesContiguousSamplesAsOneRun(t *testing.T) {
	ctx := context.Background()
	w, h := uint32(320), uint32(240)
	var blocks []genBlock
	for i := 0; i < 100; i++ {
		f := make([]byte, 4000)
		copy(f, iframeTestFrame(i, i%25 == 0))
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: f})
	}
	mkvSrc := buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}, blocks)
	src := filepath.Join(t.TempDir(), "v.mp4")
	if err := RemuxToMP4(ctx, mkvSrc, src); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := plan.Resource(ctx, "seg00001.m4s")
	if err != nil {
		t.Fatal(err)
	}
	hd, err := plan.Open(ctx, "seg00001.m4s")
	if err != nil {
		t.Fatal(err)
	}
	var got takeWriter
	if _, err := hd.WriteTo(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatal("streamed bytes differ from Resource")
	}
	if media := int64(50 * 4000); got.fromFile != media {
		t.Errorf("%d of the segment's %d sample bytes taken from the file, want all of them in one run", got.fromFile, media)
	}
}

// countingSeeker counts the reads of a source.
type countingSeeker struct {
	io.ReadSeeker
	reads int
}

func (c *countingSeeker) Read(p []byte) (int, error) {
	c.reads++
	return c.ReadSeeker.Read(p)
}

// An exact copier reads runs separated by holes of at most exactReadGap in one
// read, bounded by its buffer, and copies the same bytes as one read per run.
func TestExactCopierReadsThroughSmallGaps(t *testing.T) {
	src := make([]byte, 1<<20)
	rand.New(rand.NewSource(1)).Read(src)
	var pieces []span
	var want []byte
	for off := int64(0); off+200 <= int64(len(src)); off += 3200 { // 200-byte runs, 3000-byte holes
		pieces = append(pieces, span{off: off, n: 200})
		want = append(want, src[off:off+200]...)
	}
	copyAll := func(gap int64) ([]byte, int) {
		defer func(old int64) { exactReadGap = old }(exactReadGap)
		exactReadGap = gap
		cs := &countingSeeker{ReadSeeker: bytes.NewReader(src)}
		c := &spanCopier{src: cs, buf: make([]byte, streamBufBytes), exact: true}
		var out bytes.Buffer
		for i, pc := range pieces {
			c.reach = exactReach(pieces, i, len(c.buf))
			if err := c.copy(&out, pc.off, pc.n); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.flush(&out); err != nil {
			t.Fatal(err)
		}
		return out.Bytes(), cs.reads
	}
	exact, nExact := copyAll(0)
	through, nThrough := copyAll(64 << 10)
	if !bytes.Equal(exact, want) || !bytes.Equal(through, want) {
		t.Fatal("copied bytes differ from the runs")
	}
	if nExact != len(pieces) || nThrough > len(src)/streamBufBytes+1 {
		t.Errorf("%d runs: %d reads one run at a time (want %d), %d through the holes (want at most %d)",
			len(pieces), nExact, len(pieces), nThrough, len(src)/streamBufBytes+1)
	}
	if _, n := copyAll(2000); n != len(pieces) {
		t.Errorf("holes wider than exactReadGap: %d reads, want one per run (%d)", n, len(pieces))
	}
}

// interleaved.mp4: 12 s of 320x240 H.264 at 150 kb/s and mono AAC at 48 kb/s,
// written by a mainstream muxer that interleaves sample by sample (564 audio
// samples in 299 runs, holes of 0.6 KiB median, 3.7 KiB at most).
//
//	ffmpeg -f lavfi -i testsrc2=size=320x240:rate=25 -f lavfi -i sine=frequency=440:sample_rate=48000 -t 12 \
//	  -c:v libx264 -preset veryfast -b:v 150k -g 50 -c:a aac -b:a 48k -ac 1 -map_metadata -1 \
//	  -fflags +bitexact -flags:v +bitexact -flags:a +bitexact interleaved.mp4
const interleavedMP4 = "../internal/testdata/interleaved.mp4"

// Streamed through Open, an audio segment of a source interleaved sample by
// sample is read in one or two reads through the video frames between its
// samples, not one per run, and every resource keeps Resource's bytes.
func TestOpenInterleavedMP4ReadsAudioThroughGaps(t *testing.T) {
	ctx := context.Background()
	open := func(gap int64) (*HLSPlan, *readTally) {
		exactReadGap = gap
		tally := &readTally{}
		plan, err := PlanHLS(ctx, interleavedMP4, Options{SegmentMs: 6000, FS: countingFS(tally), StreamFromFS: true})
		if err != nil {
			t.Fatal(err)
		}
		return plan, tally
	}
	defer func(old int64) { exactReadGap = old }(exactReadGap)
	reads := func(gap int64, name string) (int, []byte) {
		plan, tally := open(gap)
		want, _, err := plan.Resource(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		h, err := plan.Open(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		before := tally.reads
		var got bytes.Buffer
		if _, err := h.WriteTo(ctx, &got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("%s (gap %d): streamed bytes differ from Resource", name, gap)
		}
		return tally.reads - before, got.Bytes()
	}
	for _, name := range []string{"seg_a1_00001.m4s", "seg_a1_00002.m4s"} {
		exact, a := reads(0, name)
		through, b := reads(64<<10, name)
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: the bytes depend on the read rule", name)
		}
		t.Logf("%s: %d reads one run at a time, %d through the gaps", name, exact, through)
		if exact < 50 || through > 2 {
			t.Errorf("%s: %d reads one run at a time, %d through the gaps; want many, then at most 2", name, exact, through)
		}
	}
	plan, _ := open(64 << 10)
	assertOpenMatchesResource(t, plan)
	if st := plan.Stats(); st.StreamedSegments == 0 || st.StreamFallbacks != 0 {
		t.Errorf("stats: %+v", st)
	}
}

// A header-stripped track streams its prefix before each sample's bytes.
func TestOpenStreamsHeaderStripping(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var blocks []genBlock
	for i := 0; i < 100; i++ {
		f := iframeTestFrame(i, i%25 == 0)
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: f[4:]}) // the 00 00 00 01 start code stripped
	}
	src := buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h,
		Compression: mkv.CompressionHeaderStrip, HeaderStripping: []byte{0, 0, 0, 1}}}, blocks)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	assertOpenMatchesResource(t, plan)
}

// Plans that cannot stream (a custom FS without StreamFromFS)
// serve Open from memory, counted as fallbacks, bytes still identical.
func TestOpenFallsBackWhenNotStreamable(t *testing.T) {
	src := openGOPSource(t, 3, false, true)
	ctx := context.Background()
	tally := &readTally{}
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs, FS: countingFS(tally)})
	if err != nil {
		t.Fatal(err)
	}
	assertOpenMatchesResource(t, plan)
	if st := plan.Stats(); st.StreamFallbacks == 0 || st.StreamedSegments != 0 {
		t.Errorf("custom FS without StreamFromFS must fall back: %+v", st)
	}
	streaming, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs, FS: countingFS(tally), StreamFromFS: true})
	if err != nil {
		t.Fatal(err)
	}
	assertOpenMatchesResource(t, streaming)
	if st := streaming.Stats(); st.StreamedSegments+st.KeptSegments == 0 || st.StreamFallbacks != 0 {
		t.Errorf("StreamFromFS must stream: %+v", st)
	}
}

// The copier reproduces arbitrary ranges of a source, reading through small
// holes and seeking over large ones, with a buffer smaller than the ranges.
func TestSpanCopier(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	src := make([]byte, 3<<20)
	rng.Read(src)
	c := spanCopier{src: bytes.NewReader(src), buf: make([]byte, 4096)}
	pos := int64(0)
	for i := 0; i < 200; i++ {
		gap := rng.Int63n(200 << 10) // holes both under and over the merge gap
		n := rng.Int63n(20 << 10)
		off := pos + gap
		if off+n > int64(len(src)) {
			break
		}
		var out bytes.Buffer
		before := c.written
		if err := c.copy(&out, off, n); err != nil {
			t.Fatalf("copy(%d,%d): %v", off, n, err)
		}
		if err := c.flush(&out); err != nil || c.written-before != n {
			t.Fatalf("copy(%d,%d): written=%d err=%v", off, n, c.written-before, err)
		}
		if !bytes.Equal(out.Bytes(), src[off:off+n]) {
			t.Fatalf("copy(%d,%d) differs", off, n)
		}
		pos = off + n
	}
	var out bytes.Buffer
	if err := c.copy(&out, int64(len(src))-10, 20); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("past the end: err=%v", err)
	}
}

// A write that fails midway reports it short, never complete.
func TestWriteRangeStopsOnWriterError(t *testing.T) {
	src := openGOPSource(t, 3, false, false)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	h, err := plan.Open(context.Background(), "seg00001.m4s")
	if err != nil {
		t.Fatal(err)
	}
	fw := &failAfter{n: 100}
	m, err := h.WriteTo(context.Background(), fw)
	if err == nil || m >= h.Size() {
		t.Fatalf("wrote %d of %d, err %v", m, h.Size(), err)
	}
}

type failAfter struct{ n, seen int }

func (f *failAfter) Write(p []byte) (int, error) {
	if f.seen+len(p) > f.n {
		k := f.n - f.seen
		f.seen = f.n
		return k, errors.New("client gone")
	}
	f.seen += len(p)
	return len(p), nil
}

// The master declares trick play on demand - resolution, codecs and URI as the
// full pass does, the bitrate the variant's peak, never below the I-frames' -
// and keeps its bytes whether or not the I-frame playlist has been built.
func TestOpenMasterDeclaresIframesOnDemand(t *testing.T) {
	src := openGOPSource(t, 6, false, true)
	ctx := context.Background()
	dir := t.TempDir()
	if err := RemuxToHLS(ctx, src, dir, Options{SegmentMs: openGOPMs}); err != nil {
		t.Fatal(err)
	}
	full, _ := os.ReadFile(filepath.Join(dir, "master.m3u8"))
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	early := plan.MasterPlaylist()
	if !bytes.Contains(early, []byte("#EXT-X-I-FRAME-STREAM-INF:")) {
		t.Fatalf("on-demand master lacks the trick-play line before any I-frame request:\n%s", early)
	}
	pl, _, err := plan.Resource(ctx, "iframe.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	lineOf := func(b []byte, prefix string) string {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, prefix) {
				return l
			}
		}
		return ""
	}
	line := func(b []byte) string { return lineOf(b, "#EXT-X-I-FRAME-STREAM-INF:") }
	bw := func(l string) int64 {
		n, _ := strconv.ParseInt(strings.TrimPrefix(bwHLS.FindString(l), "BANDWIDTH="), 10, 64)
		return n
	}
	declared, variant := bw(line(early)), bw(lineOf(early, "#EXT-X-STREAM-INF:"))
	if declared != variant {
		t.Errorf("I-frame BANDWIDTH %d, want the variant's %d", declared, variant)
	}
	if peak := iframePeakBps(t, plan, pl); declared < peak || peak <= 0 {
		t.Errorf("I-frame BANDWIDTH %d below the I-frames' measured peak %d", declared, peak)
	}
	strip := func(l string) string { return bwHLS.ReplaceAllString(l, "") }
	if got, want := strip(line(early)), strip(line(full)); got != want || got == "" {
		t.Errorf("trick-play line differs from the full pass beyond BANDWIDTH:\n plan %s\n full %s", got, want)
	}
	if !bytes.Equal(plan.MasterPlaylist(), early) {
		t.Errorf("the master changed after the I-frame build")
	}
}

var bwHLS = regexp.MustCompile(`BANDWIDTH=\d+`)

// iframePeakBps is an I-frame playlist's largest resource over its EXTINF, in bits per second.
func iframePeakBps(t *testing.T, plan *HLSPlan, pl []byte) int64 {
	t.Helper()
	var dur, peak float64
	for _, l := range strings.Split(string(pl), "\n") {
		switch {
		case strings.HasPrefix(l, "#EXTINF:"):
			dur, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(l, "#EXTINF:"), ","), 64)
		case strings.HasPrefix(l, "iframe"):
			b, _, err := plan.Resource(context.Background(), l)
			if err != nil {
				t.Fatal(err)
			}
			if n := float64(len(b)); dur > 0 && n*8/dur > peak {
				peak = n * 8 / dur
			}
		}
	}
	return int64(peak)
}

// A streamed segment's ETag carries the source's stamp: the same name and size
// over a replaced file (another mtime) yield another ETag; the same file, the same.
func TestOpenETagFollowsSource(t *testing.T) {
	src := openGOPSource(t, 3, false, false)
	ctx := context.Background()
	a, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	ha, _ := a.Open(ctx, "seg00001.m4s")
	if ha.Bytes() != nil {
		t.Fatal("the video segment must stream")
	}
	b, _ := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
	hb, _ := b.Open(ctx, "seg00001.m4s")
	if ha.ETag() != hb.ETag() || !strings.HasPrefix(ha.ETag(), `W/"`) {
		t.Errorf("same file, different ETags: %s vs %s", ha.ETag(), hb.ETag())
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(src, later, later); err != nil {
		t.Fatal(err)
	}
	c, _ := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
	hc, _ := c.Open(ctx, "seg00001.m4s")
	if hc.ETag() == ha.ETag() {
		t.Errorf("a touched source must change the ETag: %s", hc.ETag())
	}
}

// The tables every plan keeps stay under the process budget: two plans
// building many windows never hold more than the budget plus one table.
func TestTableCacheProcessBudget(t *testing.T) {
	// Earlier tests' plans give their tables back when collected: wait for that, so only this test's count.
	for i := 0; tableBytesTotal.Load() != 0; i++ {
		if i == 100 {
			t.Fatalf("%d bytes of tables still held by plans nothing references", tableBytesTotal.Load())
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	SetTableCacheBytes(64 << 10)
	defer SetTableCacheBytes(0)
	ctx := context.Background()
	src := openGOPSource(t, 12, false, true)
	var plans []*HLSPlan
	for i := 0; i < 2; i++ {
		p, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, p)
	}
	var largest int64
	for n := 0; n < 12; n++ {
		for _, p := range plans {
			tb, err := p.table(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			largest = max(largest, tb.bytes)
			if total := tableBytesTotal.Load(); total > (64<<10)+largest {
				t.Fatalf("tables total %d bytes over the %d budget (largest table %d)", total, 64<<10, largest)
			}
		}
	}
	for _, p := range plans {
		if p.tabBytes != p.tabAcct.bytes || p.tabBytes < 0 {
			t.Errorf("plan accounting off: %d vs %d", p.tabBytes, p.tabAcct.bytes)
		}
	}
}

// prefetch asks for a dense rendition prefetchAhead in front of its copy, and
// for each read of an exact one at once (runs gathered through holes up to
// exactReadGap).
func TestPrefetchRanges(t *testing.T) {
	type rng struct{ off, n int64 }
	var will []rng
	defer func(w func(*os.File, int64, int64)) { adviseWillNeed = w }(adviseWillNeed)
	adviseWillNeed = func(_ *os.File, off, n int64) { will = append(will, rng{off, n}) }
	pieces := []span{{emit: []byte("head")}, {off: 1000, n: 100}, {off: 1200, n: 100}, {off: 1300 + 200<<10, n: 50}}
	prefetch(nil, pieces, false, streamBufBytes)
	if len(will) != 1 || will[0] != (rng{1000, 200<<10 + 350}) {
		t.Fatalf("dense: %v", will)
	}
	will = nil
	const mib = 1 << 20
	a := prefetch(nil, []span{{off: 0, n: 3 * mib}, {off: 3 * mib, n: 3 * mib}, {off: 6 * mib, n: 6 * mib}, {off: 12 * mib, n: 8 * mib}}, false, streamBufBytes)
	for _, at := range []int64{0, 3 * mib, 6 * mib, 12 * mib} {
		a.advance(at)
	}
	if want := []rng{{0, 8 * mib}, {8 * mib, 6 * mib}, {14 * mib, 6 * mib}}; fmt.Sprint(will) != fmt.Sprint(want) {
		t.Fatalf("dense, 20 MiB: %v, want %v", will, want)
	}
	will = nil
	prefetch(nil, pieces, true, streamBufBytes)
	if len(will) != 2 || will[0] != (rng{1000, 300}) || will[1] != (rng{1300 + 200<<10, 50}) {
		t.Fatalf("exact: %v", will)
	}
}

// On a network mount an exact rendition's reads go out together into one
// buffer, and a Matroska window's light audio comes with its walk: every
// resource keeps Resource's bytes, in the clear and encrypted.
func TestOpenParallelReadsOnNetworkSource(t *testing.T) {
	copyFromSource(t) // the table semantics of a headers-only walk on a network source
	defer func(f func(*os.File) bool) { networkSource = f }(networkSource)
	networkSource = func(*os.File) bool { return true }
	mkvSrc := filepath.Join(t.TempDir(), "interleaved.mkv")
	if err := RemuxFromMP4(context.Background(), interleavedMP4, mkvSrc); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{interleavedMP4, mkvSrc} {
		for _, o := range []Options{{SegmentMs: 2000}, {SegmentMs: 2000, Encrypt: &HLSEncryption{Key: []byte("0123456789abcdef"), KeyURI: "k"}}} {
			plan, err := PlanHLS(context.Background(), src, o)
			if err != nil {
				t.Fatal(err)
			}
			assertOpenMatchesResource(t, plan)
			if src == mkvSrc { // a Matroska window's light audio rides in the table, read with the walk
				h, err := plan.Open(context.Background(), "seg_a1_00001.m4s")
				if err != nil {
					t.Fatal(err)
				}
				if h.Bytes() == nil {
					t.Error("a network source's light audio must come from its window's table")
				}
			}
		}
	}
}

// Options.SourceAccess imposes how a source is read: told network, a local
// Matroska file's light audio rides in the window table; told local, it is
// copied from the file; the bytes are the same either way.
func TestSourceAccessOption(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "interleaved.mkv")
	if err := RemuxFromMP4(ctx, interleavedMP4, src); err != nil {
		t.Fatal(err)
	}
	var want []byte
	copyFromSource(t) // under the window budget a kept window's light audio rides in the table whatever the access
	for _, c := range []struct {
		access  SourceAccess
		inTable bool
	}{{SourceNetwork, true}, {SourceLocal, false}, {SourceAuto, false}} {
		plan := mustPlan(t, src, Options{SegmentMs: 2000, SourceAccess: c.access})
		h, err := plan.Open(ctx, "seg_a1_00001.m4s")
		if err != nil {
			t.Fatal(err)
		}
		if (h.Bytes() != nil) != c.inTable {
			t.Errorf("SourceAccess %d: audio from the table %v, want %v", c.access, h.Bytes() != nil, c.inTable)
		}
		var got bytes.Buffer
		if _, err := h.WriteTo(ctx, &got); err != nil {
			t.Fatal(err)
		}
		if want == nil {
			want = got.Bytes()
		} else if !bytes.Equal(got.Bytes(), want) {
			t.Errorf("SourceAccess %d: the bytes differ", c.access)
		}
	}
}
