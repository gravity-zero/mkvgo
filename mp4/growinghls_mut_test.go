package mp4

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// growinghls_mut_test.go targets gremlins survivors in growinghls.go: the
// SegmentMs default fallback, Refresh's newly-published delta, the grid-probe
// index boundary, the timescale-conversion guard, the final/growing segment
// duration arithmetic, the media-playlist target-duration/EXT-X-MAP branches,
// segmentDeclaredEnd's header-validation branches, and boundedReadSeeker's
// clip arithmetic. Each test asserts the exact value/text a branch controls,
// reusing buildGrowingFixture/scanAllClusters from growinghls_test.go.

// TestGrowingHLSMutDefaultSegmentMs proves growinghls.go:170 (segMs <= 0
// boundary): Options.SegmentMs left at its zero value must fall back to the 6s
// default, not stick at 0 (which would open a new segment at every keyframe).
// The fixture's keyframes are 1s apart, so the 6s default yields exactly 2
// boundaries (0 and 6000) over the 10s presentation; a stuck-at-0 target would
// yield one boundary per keyframe (10).
func TestGrowingHLSMutDefaultSegmentMs(t *testing.T) {
	data := buildGrowingFixture(t)
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data)

	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	plan.Complete()
	if n := plan.NumSegments(); n != 2 {
		t.Fatalf("NumSegments = %d, want 2 (keyframes every 1s, default 6s segment target)", n)
	}
}

// TestGrowingHLSMutRefreshReturnsDelta proves growinghls.go:369 (published -
// before invert-negatives/arithmetic-base): Refresh must return the DELTA of
// newly published segments, not the sum. With no growth between two Refresh
// calls and at least one already-published segment, a second Refresh must
// report exactly 0 new segments - published + before would report a large
// nonzero number instead.
func TestGrowingHLSMutRefreshReturnsDelta(t *testing.T) {
	data := buildGrowingFixture(t)
	clusters := scanAllClusters(t, data)
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data[:clusters[2].end])

	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if plan.NumSegments() == 0 {
		t.Fatal("need at least one published segment before the no-growth Refresh")
	}
	n, err := plan.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("Refresh with no growth returned %d newly published segments, want 0 (published - before)", n)
	}
}

// TestGrowingHLSMutResolveHeadGridBoundary proves growinghls.go:462 (int64(i)
// < pr.frames negation): the grid-probe's blockPtsAt callback must return the
// first timecode for indices strictly below frames and the second timecode
// only at index frames itself. Called directly (bypassing a full scan) with a
// resolved probe (8 frames, TCs 1000 and 9000, ms timescale): the correct
// stride is (9000-1000+4)/8 = 1000. Under the boundary flip (<=), every index
// including the last would read the first TC, deriveGridTS would see no lace
// collapse, and gridTS would come out 0.
func TestGrowingHLSMutResolveHeadGridBoundary(t *testing.T) {
	pt := &planTrack{ft: &fragTrack{timescale: movieTimescale}}
	pr := &growingGridProbe{firstTC: 1000, frames: 8, secondTC: 9000, haveFirst: true, haveSecond: true}
	p := &GrowingHLSPlan{
		full:       &HLSPlan{tracks: []*planTrack{pt}},
		headProbes: map[*planTrack]*growingGridProbe{pt: pr},
	}
	p.resolveHeadLocked()
	if pt.gridTS != 1000 {
		t.Fatalf("gridTS = %d, want 1000 (8-frame lace stride: (9000-1000+4)/8)", pt.gridTS)
	}
	if !p.headDone {
		t.Fatal("resolveHeadLocked must set headDone")
	}
}

// TestGrowingHLSMutFinalizeTimescaleConversion proves growinghls.go:564
// (ft.timescale != movieTimescale negation): an audio track's media-timescale
// duration must be rescaled to the movie's millisecond timescale. The
// fixture's audio track runs at 48kHz; if the conversion were skipped (the
// negated guard never firing for a genuinely different timescale), durMovieMs
// would be left in 48kHz ticks - roughly 48x the ~10000ms the fixture covers.
func TestGrowingHLSMutFinalizeTimescaleConversion(t *testing.T) {
	data := buildGrowingFixture(t)
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data)

	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	plan.Complete()
	if len(plan.full.tracks) != 2 {
		t.Fatalf("fixture must have exactly 2 tracks (video+audio), got %d", len(plan.full.tracks))
	}
	vi := plan.full.videoIndex()
	ai := 1 - vi
	dur := plan.full.tracks[ai].ft.durMovieMs
	if dur < 9000 || dur > 11000 {
		t.Fatalf("audio durMovieMs = %d, want ~10000 (ms, not un-rescaled 48kHz ticks)", dur)
	}
}

// TestGrowingHLSMutFinalizeDurs proves growinghls.go:583's ARITHMETIC_BASE
// survivor (end-p.full.bounds[k] -> end+p.full.bounds[k]): once finalized,
// every published segment's duration must be the DIFFERENCE between
// consecutive bounds (or the tail end), not their sum. The fixture's 1s
// keyframes with a 2s segment target produce boundaries exactly 2000ms apart
// (0/2000/4000/6000/8000) and a tail ending at 10000ms, so every one of the 5
// finalized segments must read exactly 2.000s; summing bounds instead would
// make every segment after the first far larger (6.000, 10.000, ...).
func TestGrowingHLSMutFinalizeDurs(t *testing.T) {
	data := buildGrowingFixture(t)
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data)

	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	plan.Complete()
	pl := string(plan.MediaPlaylist())
	if !strings.Contains(pl, "#EXT-X-ENDLIST") {
		t.Fatalf("plan did not finalize:\n%s", pl)
	}
	want := plan.NumSegments()
	if want < 3 {
		t.Fatalf("need at least 3 finalized segments, got %d", want)
	}
	if got := strings.Count(pl, "#EXTINF:2.000,\n"); got != want {
		t.Fatalf("exact-2.000s EXTINF entries = %d, want %d (every segment: bounds[k+1]-bounds[k], not the sum):\n%s", got, want, pl)
	}
}

// TestGrowingHLSMutRebuildDursWhileGrowing proves growinghls.go:605's
// ARITHMETIC_BASE survivor (the same subtraction, but in the NOT-yet-finalized
// branch of rebuildOutputsLocked): while still growing (EVENT, no ENDLIST),
// every published segment's duration must likewise be bounds[k+1]-bounds[k].
// The file is grown to just short of its final cluster so Cues is never
// reached and the plan stays open.
func TestGrowingHLSMutRebuildDursWhileGrowing(t *testing.T) {
	data := buildGrowingFixture(t)
	clusters := scanAllClusters(t, data)
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data[:clusters[len(clusters)-2].end])

	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	pl := string(plan.MediaPlaylist())
	if !strings.Contains(pl, "#EXT-X-PLAYLIST-TYPE:EVENT") || strings.Contains(pl, "#EXT-X-ENDLIST") {
		t.Fatalf("expected an still-growing EVENT playlist:\n%s", pl)
	}
	want := plan.NumSegments()
	if want < 3 {
		t.Fatalf("need at least 3 published segments while growing, got %d", want)
	}
	if got := strings.Count(pl, "#EXTINF:2.000,\n"); got != want {
		t.Fatalf("exact-2.000s EXTINF entries = %d, want %d (bounds[k+1]-bounds[k], not the sum):\n%s", got, want, pl)
	}
}

// TestGrowingHLSMutBuildGrowingMediaPlaylistTargetDuration proves
// growinghls.go:771 (d > max negation): TARGETDURATION must be the ceil of the
// LARGEST duration passed in, regardless of position in the slice.
func TestGrowingHLSMutBuildGrowingMediaPlaylistTargetDuration(t *testing.T) {
	durs := []float64{2.0, 5.5, 3.0}
	segName := func(i int) string { return fmt.Sprintf("seg%05d.m4s", i+1) }
	b := buildGrowingMediaPlaylist(nil, durs, "init.mp4", segName, false)
	mustContain(t, string(b), "#EXT-X-TARGETDURATION:6\n")
}

// TestGrowingHLSMutBuildGrowingMediaPlaylistMapURI proves growinghls.go:786
// (mapURI != "" negation): a non-empty init-segment URI must render
// EXT-X-MAP, an empty one must not.
func TestGrowingHLSMutBuildGrowingMediaPlaylistMapURI(t *testing.T) {
	segName := func(i int) string { return fmt.Sprintf("seg%05d.m4s", i+1) }
	withMap := buildGrowingMediaPlaylist(nil, []float64{2.0}, "init.mp4", segName, false)
	mustContain(t, string(withMap), `#EXT-X-MAP:URI="init.mp4"`)

	noMap := buildGrowingMediaPlaylist(nil, []float64{2.0}, "", segName, false)
	mustNotContain(t, string(noMap), "#EXT-X-MAP:URI=")
}

// TestGrowingHLSMutSegmentDeclaredEndUnknownEBMLSize proves growinghls.go:840
// (h1.Size < 0 negation): an EBML header with an unknown (all-ones) size must
// fail with an explicit error, not fall through to seek by -1 and read past a
// header that was never sized.
func TestGrowingHLSMutSegmentDeclaredEndUnknownEBMLSize(t *testing.T) {
	var buf bytes.Buffer
	if _, err := ebml.WriteElementHeader(&buf, 0x1A45DFA3, -1); err != nil {
		t.Fatal(err)
	}
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", buf.Bytes())

	_, err := segmentDeclaredEnd(fs, "in.mkv")
	if err == nil || !strings.Contains(err.Error(), "unknown-size EBML header") {
		t.Fatalf("segmentDeclaredEnd error = %v, want an explicit unknown-size EBML header error", err)
	}
}

// TestGrowingHLSMutSegmentDeclaredEndWrongSecondID proves growinghls.go:850
// (h2.ID != mkv.IDSegment negation): the element right after the EBML header
// must be a Segment; anything else must fail with an explicit error, not be
// silently accepted as one.
func TestGrowingHLSMutSegmentDeclaredEndWrongSecondID(t *testing.T) {
	var buf bytes.Buffer
	if _, err := ebml.WriteElementHeader(&buf, 0x1A45DFA3, 0); err != nil { // empty EBML header body
		t.Fatal(err)
	}
	if _, err := ebml.WriteElementHeader(&buf, 0x12345678, 4); err != nil { // not IDSegment
		t.Fatal(err)
	}
	buf.Write([]byte{0, 0, 0, 0})
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", buf.Bytes())

	_, err := segmentDeclaredEnd(fs, "in.mkv")
	if err == nil || !strings.Contains(err.Error(), "expected a Segment element") {
		t.Fatalf("segmentDeclaredEnd error = %v, want the explicit \"expected a Segment element\" error", err)
	}
}

// errGrowingHLSMutForcedSeek is returned unconditionally by
// growinghlsSeekErrFile.Seek to prove a Seek error propagates unchanged.
var errGrowingHLSMutForcedSeek = errors.New("growinghls_mut_test: forced seek failure")

// growinghlsSeekErrFile wraps a byte source whose Seek always fails, so
// segmentDeclaredEnd's "skip over the EBML header body" Seek call can be
// forced to fail deterministically.
type growinghlsSeekErrFile struct{ r *bytes.Reader }

func (f *growinghlsSeekErrFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *growinghlsSeekErrFile) Seek(int64, int) (int64, error) {
	return 0, errGrowingHLSMutForcedSeek
}
func (f *growinghlsSeekErrFile) Close() error { return nil }

// TestGrowingHLSMutSegmentDeclaredEndSeekError proves growinghls.go:843
// (err != nil negation, right after the header-skipping Seek): a Seek failure
// must propagate immediately, not be swallowed and the read continue as if it
// had succeeded.
func TestGrowingHLSMutSegmentDeclaredEndSeekError(t *testing.T) {
	var buf bytes.Buffer
	if _, err := ebml.WriteElementHeader(&buf, 0x1A45DFA3, 4); err != nil {
		t.Fatal(err)
	}
	buf.Write([]byte{0, 0, 0, 0})
	fs := &mkv.FS{Open: func(string) (mkv.ReadSeekCloser, error) {
		return &growinghlsSeekErrFile{r: bytes.NewReader(buf.Bytes())}, nil
	}}

	_, err := segmentDeclaredEnd(fs, "whatever")
	if !errors.Is(err, errGrowingHLSMutForcedSeek) {
		t.Fatalf("segmentDeclaredEnd error = %v, want the forced Seek error propagated unchanged", err)
	}
}

// TestGrowingHLSMutBoundedReadSeekerNoClipWithinLimit proves growinghls.go:882
// (CONDITIONALS_NEGATION on cur+len(p) > b.limit): a read whose buffer is
// already smaller than the room left before the limit must be passed through
// unchanged, never force-extended to reach the limit. Extending it here would
// re-slice a 4-byte buffer to 50 bytes and panic (capacity 4).
func TestGrowingHLSMutBoundedReadSeekerNoClipWithinLimit(t *testing.T) {
	data := bytes.Repeat([]byte{0xAB}, 100)
	br := &boundedReadSeeker{r: bytes.NewReader(data), limit: 50}
	buf := make([]byte, 4)
	n, err := br.Read(buf)
	if err != nil {
		t.Fatalf("Read within the limit: %v", err)
	}
	if n != 4 {
		t.Fatalf("Read within the limit returned n=%d, want 4 (must not force-clip to the limit)", n)
	}
}

// TestGrowingHLSMutBoundedReadSeekerClipsAtLimit proves growinghls.go:883
// (ARITHMETIC_BASE on b.limit-cur): when the requested read WOULD cross the
// limit, it must be clipped to exactly the room left (limit-cur), not
// re-slice using limit+cur, which would panic (re-slicing a 10-byte buffer far
// past its capacity).
func TestGrowingHLSMutBoundedReadSeekerClipsAtLimit(t *testing.T) {
	data := bytes.Repeat([]byte{0xCD}, 100)
	br := &boundedReadSeeker{r: bytes.NewReader(data), limit: 50}
	if _, err := br.Seek(48, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10) // requests more than the 2 bytes left before the limit
	n, err := br.Read(buf)
	if err != nil {
		t.Fatalf("clipped read: %v", err)
	}
	if n != 2 {
		t.Fatalf("clipped Read returned n=%d, want 2 (limit(50) - cur(48))", n)
	}
}
