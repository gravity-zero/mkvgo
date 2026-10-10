package mp4

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

// assertOpenMatchesResource opens every resource and checks the handle's size,
// its whole write and a few sub-ranges against Resource's bytes.
func assertOpenMatchesResource(t *testing.T, plan *HLSPlan) {
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
		t.Run(name, func(t *testing.T) {
			segMs := int64(2000)
			if name == "open-gop" {
				segMs = openGOPMs
			}
			plan, err := PlanHLS(ctx, src, Options{SegmentMs: segMs})
			if err != nil {
				t.Fatal(err)
			}
			assertOpenMatchesResource(t, plan)
			st := plan.Stats()
			if st.TableBuilds == 0 || st.StreamedSegments == 0 || st.StreamFallbacks != 0 {
				t.Errorf("stats: %+v", st)
			}
		})
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

// Plans that cannot stream (encryption, a custom FS without StreamFromFS)
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
	if st := streaming.Stats(); st.StreamedSegments == 0 {
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
		if m, err := c.copy(&out, off, n); err != nil || m != n {
			t.Fatalf("copy(%d,%d): m=%d err=%v", off, n, m, err)
		}
		if !bytes.Equal(out.Bytes(), src[off:off+n]) {
			t.Fatalf("copy(%d,%d) differs", off, n)
		}
		pos = off + n
	}
	var out bytes.Buffer
	if _, err := c.copy(&out, int64(len(src))-10, 20); !errors.Is(err, io.ErrUnexpectedEOF) {
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
// full pass does, the bitrate an estimate from the first keyframe - and keeps
// its bytes whether or not the I-frame playlist has been built.
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
	if _, _, err := plan.Resource(ctx, "iframe.m3u8"); err != nil {
		t.Fatal(err)
	}
	line := func(b []byte) string {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "#EXT-X-I-FRAME-STREAM-INF:") {
				return l
			}
		}
		return ""
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
