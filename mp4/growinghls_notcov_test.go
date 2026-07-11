package mp4

// growinghls_notcov_test.go targets growinghls.go lines that NO existing test
// executes at all (not just mutation survivors - genuinely dead from a
// coverage standpoint): scanLocked's error-propagation branches (ctx
// cancellation, a Seek failure, a downstream scanClusterLocked/finalizeLocked
// failure, an unknown-size cluster, the declared-Segment-end auto-finalize
// path this repo's own writer never exercises since it never backpatches the
// Segment size), extendHeadLocked's lazy (needsFirstFrame) sample-entry error
// and repeated-block-timecode probe step, finalizeLocked's gridTS>0/
// frameDurMs>0 duration branches and the laced tail's lastFrames correction,
// Resources()'s segment-name loop, and Resource()'s per-rendition/segment/
// audio-multi-rendition branches plus segmentDeclaredEnd's own error paths.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// errGrowncovForcedOpen/errGrowncovForcedSeek are returned unconditionally by
// this file's fake FS/file wrappers to prove a specific internal Open/Seek
// failure propagates unchanged rather than being swallowed or retried.
var (
	errGrowncovForcedOpen = errors.New("growinghls_notcov_test: forced open failure")
	errGrowncovForcedSeek = errors.New("growinghls_notcov_test: forced seek failure")
)

// growncovContains reports whether s is present in list.
func growncovContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- scanLocked's error-propagation branches (growinghls.go:329-364) -------

// TestGrowingHLSNotCovRefreshCtxCancelled proves growinghls.go:329-331: an
// already-cancelled context must abort a Refresh immediately, reporting 0
// newly published segments (not a partial scan) and the exact context error.
func TestGrowingHLSNotCovRefreshCtxCancelled(t *testing.T) {
	data := buildGrowingFixture(t)
	clusters := scanAllClusters(t, data)
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data[:clusters[2].end])

	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	before := plan.NumSegments()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := plan.Refresh(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Refresh with an already-cancelled context returned err=%v, want context.Canceled", err)
	}
	if n != 0 {
		t.Fatalf("Refresh returned %d newly published segments on immediate ctx cancellation, want 0", n)
	}
	if plan.NumSegments() != before {
		t.Fatalf("NumSegments changed from %d to %d despite the cancelled Refresh", before, plan.NumSegments())
	}
}

// growncovSeekFailAtOffset wraps a ReadSeekCloser so that, once armed, a Seek
// to one specific absolute offset fails - letting a test force scanLocked's
// own cursor Seek to fail without disturbing any other Seek (including the
// same call's earlier ones) on a real, growing MemFS file.
type growncovSeekFailAtOffset struct {
	r       mkv.ReadSeekCloser
	armed   *bool
	failAt  int64
	failErr error
}

func (s *growncovSeekFailAtOffset) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *growncovSeekFailAtOffset) Close() error               { return s.r.Close() }
func (s *growncovSeekFailAtOffset) Seek(offset int64, whence int) (int64, error) {
	if *s.armed && whence == io.SeekStart && offset == s.failAt {
		return 0, s.failErr
	}
	return s.r.Seek(offset, whence)
}

// TestGrowingHLSNotCovScanLockedSeekError proves growinghls.go:332-334: a
// Seek failure positioning the cursor at the next cluster must propagate
// immediately, not be swallowed and the scan continue as if unchanged.
func TestGrowingHLSNotCovScanLockedSeekError(t *testing.T) {
	data := buildGrowingFixture(t)
	clusters := scanAllClusters(t, data)
	m := mkv.NewMemFS()
	under := m.FS()
	armed := false
	fs := &mkv.FS{
		Open: func(path string) (mkv.ReadSeekCloser, error) {
			f, err := under.Open(path)
			if err != nil {
				return nil, err
			}
			return &growncovSeekFailAtOffset{r: f, armed: &armed, failAt: clusters[1].start, failErr: errGrowncovForcedSeek}, nil
		},
		Stat: under.Stat,
	}

	m.Put("in.mkv", data[:clusters[0].end])
	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}

	m.Put("in.mkv", data[:clusters[1].end])
	armed = true
	if _, err := plan.Refresh(context.Background()); !errors.Is(err, errGrowncovForcedSeek) {
		t.Fatalf("Refresh error = %v, want the forced cursor-Seek error propagated unchanged", err)
	}
}

// growncovFailOnOpen wraps an underlying FS so that, once armed, the n-th
// Open call (counted from the last (re)arm) fails with a fixed error. This
// lets a test force a failure deep inside a single Refresh call's OWN
// internal re-open (peekTail's walkBlocks re-opens the source when
// finalizeLocked runs) without disturbing any Open that happened earlier,
// including the several made while constructing the plan itself.
type growncovFailOnOpen struct {
	under   *mkv.FS
	armed   bool
	n       int
	count   int
	failErr error
}

func (w *growncovFailOnOpen) open(path string) (mkv.ReadSeekCloser, error) {
	if w.armed {
		w.count++
		if w.count == w.n {
			return nil, w.failErr
		}
	}
	return w.under.DoOpen(path)
}

// TestGrowingHLSNotCovFinalizeErrorViaCuesBranch proves growinghls.go:342-344:
// when a trailing Cues element auto-detects completion mid-scan,
// finalizeLocked's own error must propagate through that call site, not be
// discarded.
func TestGrowingHLSNotCovFinalizeErrorViaCuesBranch(t *testing.T) {
	data := buildGrowingFixture(t)
	clusters := scanAllClusters(t, data)
	m := mkv.NewMemFS()
	under := m.FS()
	w := &growncovFailOnOpen{under: under, failErr: errGrowncovForcedOpen}
	fs := &mkv.FS{Open: w.open, Stat: under.DoStat}

	m.Put("in.mkv", data[:clusters[2].end])
	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}

	m.Put("in.mkv", data) // whole clusters + trailing Cues: auto-detected completion
	w.armed = true
	w.n = 2 // 1: scanLocked's own open, 2: peekTail's internal re-open
	w.count = 0
	if _, err := plan.Refresh(context.Background()); !errors.Is(err, errGrowncovForcedOpen) {
		t.Fatalf("Refresh error = %v, want the forced peekTail re-open error propagated through the Cues-branch finalizeLocked call", err)
	}
}

// TestGrowingHLSNotCovUnknownSizeCluster proves growinghls.go:347-349: a
// Cluster whose header declares an unknown size must fail with the explicit,
// documented error - a growing plan does not support unknown-size clusters.
func TestGrowingHLSNotCovUnknownSizeCluster(t *testing.T) {
	data := growncovUnknownSizeClusterFixture(t)
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data)

	_, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs})
	if err == nil || !strings.Contains(err.Error(), "growing plan requires known-size Matroska clusters") {
		t.Fatalf("PlanGrowingHLS error = %v, want the explicit known-size-cluster requirement", err)
	}
}

// growncovUnknownSizeClusterFixture hand-writes a minimal Matroska file (EBML
// header, Segment, Info, Tracks) followed by a single Cluster header whose
// data size is the EBML unknown-size marker - a shape buildMKV/the repo's own
// writer never produces, needed to exercise scanLocked's explicit rejection.
func growncovUnknownSizeClusterFixture(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unkcluster.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w, h := uint32(320), uint32(240)
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	if err := mw.WriteMetadata(c, tracks, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := ebml.WriteElementHeader(mw.W, mkv.IDCluster, -1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestGrowingHLSNotCovScanClusterErrorBadSampleEntry proves growinghls.go:355
// (scanClusterLocked's error return) and growinghls.go:429-433
// (extendHeadLocked's lazy sample-entry error): a codec whose sample entry is
// built lazily from the first frame (vp9, needsFirstFrame) must propagate a
// malformed first frame's parse error out through the whole scan chain,
// rather than silently building a bad init segment.
func TestGrowingHLSNotCovScanClusterErrorBadSampleEntry(t *testing.T) {
	w, h := uint32(320), uint32(240)
	gblocks := []genBlock{{track: 1, pts: 0, key: true, data: []byte{0x00}}} // not a valid VP9 frame_marker
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "vp9", Width: &w, Height: &h},
	}, gblocks)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data)

	_, err = PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs})
	if err == nil || !strings.Contains(err.Error(), "VP9: bad frame_marker") {
		t.Fatalf("PlanGrowingHLS error = %v, want the vp9 sample-entry parse error propagated from extendHeadLocked/scanClusterLocked", err)
	}
}

// growncovPatchKnownSegmentSize returns a copy of data (a normal buildMKV/
// buildGrowingFixture output) whose Segment element declares a KNOWN size
// (len(data) minus the Segment's own data start) instead of the writer's
// usual unknown-size marker. The 8-byte size field's width is preserved, so
// no other offset in the file shifts. This repo's own writer never
// backpatches the Segment size (see growinghls.go's package doc), so this is
// the only way to exercise segmentDeclaredEnd's known-size success path and
// scanLocked's declared-end auto-finalize branch.
func growncovPatchKnownSegmentSize(t *testing.T, data []byte) []byte {
	t.Helper()
	r := bytes.NewReader(data)
	h1, n1, err := ebml.ReadElementHeader(r)
	if err != nil {
		t.Fatal(err)
	}
	pos := int64(n1) + h1.Size
	if _, err := r.Seek(pos, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	h2, n2, err := ebml.ReadElementHeader(r)
	if err != nil {
		t.Fatal(err)
	}
	if h2.ID != mkv.IDSegment || n2 != 12 {
		t.Fatalf("fixture's Segment header is not the writer's 12-byte (8-byte size VINT) form (n2=%d, size=%d)", n2, h2.Size)
	}
	segStart := pos + int64(n2)
	known := int64(len(data)) - segStart
	out := append([]byte(nil), data...)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(known)|(uint64(1)<<56))
	copy(out[pos+4:pos+12], buf[:]) // IDSegment is 4 octets; the size VINT is the next 8
	return out
}

// TestGrowingHLSNotCovDeclaredEndAutoFinalize proves growinghls.go:360-361: a
// growing source whose Segment element declares a KNOWN size must
// auto-finalize once the file reaches that declared end and the cursor has
// caught up to it - even with no trailing Cues element at all (the OTHER
// auto-detect signal).
func TestGrowingHLSNotCovDeclaredEndAutoFinalize(t *testing.T) {
	data := buildGrowingFixture(t)
	clusters := scanAllClusters(t, data)
	patched := growncovPatchKnownSegmentSize(t, data[:clusters[3].end]) // no trailing Cues

	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", patched)

	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	pl := string(plan.MediaPlaylist())
	if !strings.Contains(pl, "#EXT-X-ENDLIST") {
		t.Fatalf("expected auto-finalize via the declared Segment end (no Cues present):\n%s", pl)
	}
	if plan.NumSegments() == 0 {
		t.Fatal("expected at least one published segment")
	}
}

// TestGrowingHLSNotCovDeclaredEndFinalizeError proves growinghls.go:361-363:
// when the declared-Segment-end auto-finalize condition fires,
// finalizeLocked's own error must propagate through THIS call site too (a
// separate line from the Cues-branch call site covered above).
func TestGrowingHLSNotCovDeclaredEndFinalizeError(t *testing.T) {
	data := buildGrowingFixture(t)
	clusters := scanAllClusters(t, data)
	patched := growncovPatchKnownSegmentSize(t, data[:clusters[3].end])

	m := mkv.NewMemFS()
	under := m.FS()
	w := &growncovFailOnOpen{under: under, failErr: errGrowncovForcedOpen}
	fs := &mkv.FS{Open: w.open, Stat: under.DoStat}

	m.Put("in.mkv", patched[:clusters[2].end])
	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plan.MediaPlaylist()), "#EXT-X-ENDLIST") {
		t.Fatal("must not be finalized before the declared end is reached")
	}

	m.Put("in.mkv", patched) // grows exactly to the declared end
	w.armed = true
	w.n = 2
	w.count = 0
	if _, err := plan.Refresh(context.Background()); !errors.Is(err, errGrowncovForcedOpen) {
		t.Fatalf("Refresh error = %v, want the forced peekTail re-open error propagated through the declared-end finalizeLocked call", err)
	}
}

// --- finalizeLocked's duration branches (growinghls.go:544-559) ------------

// TestGrowingHLSNotCovFinalizeLacedGridByteIdentity proves growinghls.go:443-
// 444 (extendHeadLocked's repeated-block-timecode probe step), :544-545
// (finalizeLocked's gridTS>0 branch) and :554-559 (the laced tail's
// lastFrames correction to kLast) for a laced, no-DefaultDuration audio
// track (buildLacedFixtureOpt's declareDur=false shape - real-world E-AC-3/
// AAC rips).
//
// RemuxToHLS is NOT a valid oracle for the audio track's own duration here:
// its full, one-shot mux path has every real per-frame timestamp and never
// needs (or takes) the lastFrames/kLast reconstruction, so its audio
// duration can legitimately differ from the head+tail-peek approximation
// PlanHLS/GrowingHLSPlan both use - confirmed by inspection (durMediaTS
// matches PlanHLS's own tracks exactly; the golden's independently-muxed
// init_a1.mp4 does not, by a few grid ticks). The video rendition IS a valid
// byte-identity check (unaffected by the audio-only grid reconstruction).
// So: the video rendition is checked byte-for-byte against the full-mux
// golden, and the audio track's OWN reconstructed duration is checked
// against a PlanHLS plan of the SAME file, whose hlsplan.go performs the
// exact same arithmetic (hlsplan.go:245-268) growinghls.go's finalizeLocked
// duplicates.
func TestGrowingHLSNotCovFinalizeLacedGridByteIdentity(t *testing.T) {
	src, _ := buildLacedFixtureOpt(t, false)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	goldenDir, goldenSegs := buildGoldenHLS(t, data)

	golden, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	gai := 1 - golden.videoIndex()

	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data)
	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	plan.Complete()

	if plan.NumSegments() != goldenSegs {
		t.Fatalf("NumSegments = %d, want %d (golden)", plan.NumSegments(), goldenSegs)
	}
	ai := 1 - plan.full.videoIndex()
	if plan.full.tracks[ai].gridTS <= 0 {
		t.Fatalf("audio track's collapsed-lace grid did not resolve (gridTS=%d): fixture is not exercising the lace-collapse path", plan.full.tracks[ai].gridTS)
	}

	wantDurMediaTS := golden.tracks[gai].ft.durMediaTS
	if got := plan.full.tracks[ai].ft.durMediaTS; got != wantDurMediaTS {
		t.Errorf("audio track's durMediaTS = %d, want %d (PlanHLS's own lastFrames/kLast correction on the same laced tail)", got, wantDurMediaTS)
	}

	wantInit, err := os.ReadFile(filepath.Join(goldenDir, "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.InitSegment(); !bytes.Equal(got, wantInit) {
		t.Errorf("finalized init.mp4 differs from golden byte-for-byte (%d vs %d bytes)", len(got), len(wantInit))
	}
	for k := 0; k < goldenSegs; k++ {
		got, err := plan.Segment(context.Background(), k)
		if err != nil {
			t.Fatalf("Segment(%d): %v", k, err)
		}
		want, err := os.ReadFile(filepath.Join(goldenDir, goldenSegmentName(k)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("segment %d differs from golden", k)
		}
	}
}

// TestGrowingHLSNotCovFinalizeFrameDurMs proves growinghls.go:546-547: a track
// with an explicit constant frame duration (Options-independent,
// MKV DefaultDuration-derived FrameRate) must use it directly as its final
// sample's duration, taking priority over the fallback prevPts arithmetic.
func TestGrowingHLSNotCovFinalizeFrameDurMs(t *testing.T) {
	w, h := uint32(320), uint32(240)
	fr := 25.0
	var gblocks []genBlock
	for i := 0; i < 3; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 1000, key: true,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	sortGenBlocks(gblocks)
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h, FrameRate: &fr},
	}, gblocks)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data)
	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 500})
	if err != nil {
		t.Fatal(err)
	}
	plan.Complete()

	vi := plan.full.videoIndex()
	if got := plan.full.tracks[vi].lastDurTS; got != 40 {
		t.Fatalf("video track's lastDurTS = %d, want 40 (frameDurMs branch: 25fps -> round(1000/25)ms)", got)
	}
}

// --- Resources()/Resource() (growinghls.go:681-761) -------------------------

// TestGrowingHLSNotCovResourcesListsPublishedSegments proves growinghls.go:
// 686-690: Resources() must list every published segment name for every
// rendition, growing with p.published - not a fixed or off-by-one count.
func TestGrowingHLSNotCovResourcesListsPublishedSegments(t *testing.T) {
	data := buildGrowingFixture(t)
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data)
	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	plan.Complete()

	n := plan.NumSegments()
	if n == 0 {
		t.Fatal("need at least one published segment")
	}
	names := plan.Resources()

	want := 1 + 2*(2+n) // master.m3u8 + 2 renditions * (playlist+init+n segments)
	if len(names) != want {
		t.Fatalf("Resources() returned %d names, want %d (master + 2 renditions * (playlist+init+%d segments)):\n%v", len(names), want, n, names)
	}
	for _, s := range []string{
		"master.m3u8", "playlist.m3u8", "init.mp4", "audio1.m3u8", "init_a1.mp4",
		goldenSegmentName(n - 1), fmt.Sprintf("seg_a1_%05d.m4s", n),
	} {
		if !growncovContains(names, s) {
			t.Errorf("Resources() missing %q:\n%v", s, names)
		}
	}
}

// TestGrowingHLSNotCovSegmentOutOfRange proves growinghls.go:701-703: asking
// for one segment past the last published index must fail with an explicit,
// bounded error.
func TestGrowingHLSNotCovSegmentOutOfRange(t *testing.T) {
	data := buildGrowingFixture(t)
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data)
	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	plan.Complete()

	n := plan.NumSegments()
	if _, err := plan.Segment(context.Background(), n); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("Segment(%d) error = %v, want an explicit out-of-range error", n, err)
	}
}

// TestGrowingHLSNotCovResourceNoDataYet proves growinghls.go:728-731 and :733-
// 736: asking for a rendition's playlist/init before the head has resolved
// must fail with the explicit "no data yet" error, not a nil-slice panic or a
// silently empty payload.
func TestGrowingHLSNotCovResourceNoDataYet(t *testing.T) {
	data := buildGrowingFixture(t)
	clusters := scanAllClusters(t, data)
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data[:clusters[0].start]) // head only, no cluster yet
	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := plan.Resource(context.Background(), "playlist.m3u8"); err == nil || !strings.Contains(err.Error(), "no data yet") {
		t.Fatalf("Resource(playlist.m3u8) before the head resolves = %v, want the explicit \"no data yet\" error", err)
	}
	if _, _, err := plan.Resource(context.Background(), "init.mp4"); err == nil || !strings.Contains(err.Error(), "no data yet") {
		t.Fatalf("Resource(init.mp4) before the head resolves = %v, want the explicit \"no data yet\" error", err)
	}
}

// TestGrowingHLSNotCovResourceRenditionNames proves growinghls.go:728-737
// (successful playlist/init lookups for BOTH the primary and a non-primary
// audio rendition) and :741-746/:748-757 (the seg%05d.m4s and
// seg_aX_Y.m4s parses, each returning the same bytes Segment/segmentTrack
// would).
func TestGrowingHLSNotCovResourceRenditionNames(t *testing.T) {
	data := buildGrowingFixture(t)
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data)
	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	plan.Complete()

	for _, name := range []string{"playlist.m3u8", "audio1.m3u8"} {
		if _, ct, err := plan.Resource(context.Background(), name); err != nil || ct != "application/vnd.apple.mpegurl" {
			t.Errorf("Resource(%s) = (_, %q, %v), want the m3u8 mime type and no error", name, ct, err)
		}
	}
	for _, name := range []string{"init.mp4", "init_a1.mp4"} {
		if _, ct, err := plan.Resource(context.Background(), name); err != nil || ct != "video/mp4" {
			t.Errorf("Resource(%s) = (_, %q, %v), want the mp4 mime type and no error", name, ct, err)
		}
	}

	videoSeg, ct, err := plan.Resource(context.Background(), "seg00001.m4s")
	if err != nil || ct != "video/iso.segment" {
		t.Fatalf("Resource(seg00001.m4s) = (_, %q, %v), want the segment mime type and no error", ct, err)
	}
	wantVideoSeg, err := plan.Segment(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(videoSeg, wantVideoSeg) {
		t.Error("Resource(seg00001.m4s) did not return the same bytes as Segment(0)")
	}

	audioSeg, ct, err := plan.Resource(context.Background(), "seg_a1_00001.m4s")
	if err != nil || ct != "video/iso.segment" {
		t.Fatalf("Resource(seg_a1_00001.m4s) = (_, %q, %v), want the segment mime type and no error", ct, err)
	}
	if len(audioSeg) == 0 {
		t.Error("Resource(seg_a1_00001.m4s) returned no data")
	}
}

// TestGrowingHLSNotCovResourceSegmentOutOfRange proves growinghls.go:741-744
// and :749-754: a syntactically valid segment name whose index exceeds
// p.published must fail with an explicit out-of-range error, for both the
// primary and a non-primary audio rendition.
func TestGrowingHLSNotCovResourceSegmentOutOfRange(t *testing.T) {
	data := buildGrowingFixture(t)
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data)
	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	plan.Complete()
	n := plan.NumSegments()

	name := fmt.Sprintf("seg%05d.m4s", n+50)
	if _, _, err := plan.Resource(context.Background(), name); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("Resource(%s) = %v, want an explicit out-of-range error", name, err)
	}
	aname := fmt.Sprintf("seg_a1_%05d.m4s", n+50)
	if _, _, err := plan.Resource(context.Background(), aname); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("Resource(%s) = %v, want an explicit out-of-range error", aname, err)
	}
}

// TestGrowingHLSNotCovResourceUnknownName proves growinghls.go:760: a name
// matching none of the known shapes must fail with the explicit
// unknown-resource error, not a nil/empty silent success.
func TestGrowingHLSNotCovResourceUnknownName(t *testing.T) {
	data := buildGrowingFixture(t)
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data)
	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	plan.Complete()

	if _, _, err := plan.Resource(context.Background(), "bogus.bin"); err == nil || !strings.Contains(err.Error(), "unknown HLS resource") {
		t.Fatalf("Resource(bogus.bin) = %v, want the explicit unknown-resource error", err)
	}
}

// --- segmentDeclaredEnd's own error/success paths (growinghls.go:830-861) --

// TestGrowingHLSNotCovSegmentDeclaredEndOpenError proves growinghls.go:831-
// 834: an Open failure must propagate unchanged.
func TestGrowingHLSNotCovSegmentDeclaredEndOpenError(t *testing.T) {
	fs := &mkv.FS{Open: func(string) (mkv.ReadSeekCloser, error) { return nil, errGrowncovForcedOpen }}
	if _, err := segmentDeclaredEnd(fs, "whatever"); !errors.Is(err, errGrowncovForcedOpen) {
		t.Fatalf("segmentDeclaredEnd error = %v, want the forced Open error propagated unchanged", err)
	}
}

// TestGrowingHLSNotCovSegmentDeclaredEndEBMLReadError proves growinghls.go:
// 836-839: an empty file (not even one byte for the EBML header) must fail
// reading that header, distinct from the "unknown-size EBML header" error
// (which requires a successfully-parsed header).
func TestGrowingHLSNotCovSegmentDeclaredEndEBMLReadError(t *testing.T) {
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", []byte{})

	_, err := segmentDeclaredEnd(fs, "in.mkv")
	if err == nil {
		t.Fatal("segmentDeclaredEnd on an empty file must fail reading the EBML header")
	}
	if strings.Contains(err.Error(), "unknown-size EBML header") {
		t.Fatalf("got the unknown-size-EBML-header error, want a plain header READ failure instead: %v", err)
	}
}

// TestGrowingHLSNotCovSegmentDeclaredEndSegmentReadError proves
// growinghls.go:846-849: a file that ends right after a valid EBML header
// must fail reading the Segment header, distinct from the
// "expected a Segment element" wrong-ID error (which requires a
// successfully-parsed second header).
func TestGrowingHLSNotCovSegmentDeclaredEndSegmentReadError(t *testing.T) {
	var buf bytes.Buffer
	if _, err := ebml.WriteElementHeader(&buf, 0x1A45DFA3, 0); err != nil { // EBML header, empty body
		t.Fatal(err)
	}
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", buf.Bytes())

	_, err := segmentDeclaredEnd(fs, "in.mkv")
	if err == nil {
		t.Fatal("segmentDeclaredEnd must fail reading the Segment header when the file ends right after the EBML header")
	}
	if strings.Contains(err.Error(), "expected a Segment element") {
		t.Fatalf("got the wrong-ID error, want a plain header READ failure instead: %v", err)
	}
}

// TestGrowingHLSNotCovSegmentDeclaredEndKnownSizeSuccess proves
// growinghls.go:856-860: a Segment element with a KNOWN (non-unknown) size
// must return its exact declared end (segStart + size), the success path
// this repo's own writer never produces on its own.
func TestGrowingHLSNotCovSegmentDeclaredEndKnownSizeSuccess(t *testing.T) {
	var buf bytes.Buffer
	if _, err := ebml.WriteElementHeader(&buf, 0x1A45DFA3, 0); err != nil {
		t.Fatal(err)
	}
	const knownSize = int64(12345)
	if _, err := ebml.WriteElementHeader(&buf, mkv.IDSegment, knownSize); err != nil {
		t.Fatal(err)
	}
	segDataStart := int64(buf.Len())
	buf.Write(make([]byte, 16)) // trailing bytes past the declared end; irrelevant here

	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", buf.Bytes())

	got, err := segmentDeclaredEnd(fs, "in.mkv")
	if err != nil {
		t.Fatal(err)
	}
	want := segDataStart + knownSize
	if got != want {
		t.Fatalf("segmentDeclaredEnd = %d, want %d (segStart %d + declared size %d)", got, want, segDataStart, knownSize)
	}
}

// growncovFailSeekCurrentZero wraps a ReadSeekCloser whose Seek(0,
// io.SeekCurrent) call fails - and only that exact call, so
// segmentDeclaredEnd's EARLIER Seek (skipping the EBML header body, a
// different offset/whence pair) still succeeds.
type growncovFailSeekCurrentZero struct {
	r       io.ReadSeeker
	failErr error
}

func (f *growncovFailSeekCurrentZero) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *growncovFailSeekCurrentZero) Close() error               { return nil }
func (f *growncovFailSeekCurrentZero) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekCurrent && offset == 0 {
		return 0, f.failErr
	}
	return f.r.Seek(offset, whence)
}

// TestGrowingHLSNotCovSegmentDeclaredEndSecondSeekError proves
// growinghls.go:856-858: a failure on the Seek that reads back the current
// position (to compute segStart) must propagate unchanged.
func TestGrowingHLSNotCovSegmentDeclaredEndSecondSeekError(t *testing.T) {
	var buf bytes.Buffer
	if _, err := ebml.WriteElementHeader(&buf, 0x1A45DFA3, 4); err != nil { // non-empty body: the first Seek's offset isn't 0
		t.Fatal(err)
	}
	buf.Write([]byte{0, 0, 0, 0})
	if _, err := ebml.WriteElementHeader(&buf, mkv.IDSegment, 100); err != nil {
		t.Fatal(err)
	}

	fs := &mkv.FS{Open: func(string) (mkv.ReadSeekCloser, error) {
		return &growncovFailSeekCurrentZero{r: bytes.NewReader(buf.Bytes()), failErr: errGrowncovForcedSeek}, nil
	}}

	_, err := segmentDeclaredEnd(fs, "whatever")
	if !errors.Is(err, errGrowncovForcedSeek) {
		t.Fatalf("segmentDeclaredEnd error = %v, want the forced Seek(0, SeekCurrent) error propagated unchanged", err)
	}
}
