package mp4

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestTruncatedMP4KeepsWhatArrived pins that a faststart MP4 cut short remuxes and
// serves what it holds, as players play it up to the cut, instead of being refused whole - and that the remux says
// so through OnTruncated, since its output would otherwise pass for the whole file.
func TestTruncatedMP4KeepsWhatArrived(t *testing.T) {
	ctx := context.Background()
	src := editShiftFixture(t, true)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(t.TempDir(), "cut.mp4")
	if err := os.WriteFile(cut, data[:len(data)*9/10], 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.mkv")
	var reports []Truncation
	if err := RemuxFromMP4(ctx, cut, out, Options{OnTruncated: func(tr Truncation) { reports = append(reports, tr) }}); err != nil {
		t.Fatalf("a cut MP4 was refused: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("OnTruncated called %d times, want once", len(reports))
	}
	if r := reports[0]; r.FileSize != int64(len(data)*9/10) || r.ReachedMs <= 0 || r.ReachedMs >= r.DurationMs {
		t.Errorf("truncation report %+v: want the cut file's size and a reach inside its declared duration", r)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	blocks := 0
	for {
		if _, err := br.Next(); err != nil {
			break
		}
		blocks++
	}
	if blocks == 0 {
		t.Error("the remux of a cut MP4 holds no block")
	}
	plan, err := PlanHLS(ctx, cut, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatalf("HLS refused a cut MP4: %v", err)
	}
	if _, err := plan.Segment(ctx, 0); err != nil {
		t.Errorf("first segment of a cut MP4: %v", err)
	}
}

// TestWholeMP4ReportsNoTruncation pins the other side: a whole file never calls OnTruncated.
func TestWholeMP4ReportsNoTruncation(t *testing.T) {
	called := false
	out := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), editShiftFixture(t, true), out, Options{OnTruncated: func(Truncation) { called = true }}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("OnTruncated called on a whole file")
	}
}

// TestVerifyCutShortMP4 pins that a hashed MP4 cut short fails verification as a missing tail, not as corruption.
func TestVerifyCutShortMP4(t *testing.T) {
	src := buildMKV(t, synthFixtureTracks(), synthFixtureBlocks(0))
	hashed := filepath.Join(t.TempDir(), "hashed.mp4")
	if err := RemuxToMP4(context.Background(), src, hashed, Options{FastStart: true, ContentHashes: true}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(hashed)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(t.TempDir(), "cut.mp4")
	if err := os.WriteFile(cut, data[:len(data)*9/10], 0o644); err != nil {
		t.Fatal(err)
	}
	mm, err := VerifyContentHashes(context.Background(), cut)
	if err != nil {
		t.Fatal(err)
	}
	if len(mm) == 0 {
		t.Fatal("a cut hashed MP4 verified clean")
	}
	for _, m := range mm {
		if !m.CutShort {
			t.Errorf("track %d mismatch not marked CutShort: %s", m.TrackID, m)
		}
	}
	if whole, err := VerifyContentHashes(context.Background(), hashed); err != nil || len(whole) != 0 {
		t.Errorf("whole hashed MP4: %v, %v; want clean", whole, err)
	}
}

// cutCopy writes the first nine tenths of src to a new file: an incomplete file.
func cutCopy(t *testing.T, src, name string) (string, int64) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	n := len(data) * 9 / 10
	cut := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(cut, data[:n], 0o644); err != nil {
		t.Fatal(err)
	}
	return cut, int64(n)
}

// TestConversionsReportCutSource pins that the conversions of a source cut short - a Matroska that stops before
// the end its Segment declares, an MP4 whose media runs past the file - write what arrived and say so once through
// OnTruncated, and that a whole source says nothing.
func TestConversionsReportCutSource(t *testing.T) {
	ctx := context.Background()
	mkvSrc := buildMKV(t, synthFixtureTracks(), synthFixtureBlocks(0))
	mp4Src := editShiftFixture(t, true)
	cutMKV, mkvSize := cutCopy(t, mkvSrc, "cut.mkv")
	cutMP4, mp4Size := cutCopy(t, mp4Src, "cut.mp4")
	cases := []struct {
		name string
		src  string
		size int64 // 0: a whole source
		run  func(src string, o Options) error
	}{
		{"to-mp4 cut mkv", cutMKV, mkvSize, func(src string, o Options) error {
			return RemuxToMP4(ctx, src, filepath.Join(t.TempDir(), "o.mp4"), o)
		}},
		{"to-mp4 cut mkv faststart", cutMKV, mkvSize, func(src string, o Options) error {
			o.FastStart = true
			return RemuxToMP4(ctx, src, filepath.Join(t.TempDir(), "o.mp4"), o)
		}},
		{"to-mp4 whole mkv", mkvSrc, 0, func(src string, o Options) error {
			return RemuxToMP4(ctx, src, filepath.Join(t.TempDir(), "o.mp4"), o)
		}},
		{"to-hls cut mkv", cutMKV, mkvSize, func(src string, o Options) error {
			return RemuxToHLS(ctx, src, t.TempDir(), o)
		}},
		{"to-hls whole mkv", mkvSrc, 0, func(src string, o Options) error {
			return RemuxToHLS(ctx, src, t.TempDir(), o)
		}},
		{"to-hls cut mp4", cutMP4, mp4Size, func(src string, o Options) error {
			return RemuxToHLS(ctx, src, t.TempDir(), o)
		}},
		{"to-hls whole mp4", mp4Src, 0, func(src string, o Options) error {
			return RemuxToHLS(ctx, src, t.TempDir(), o)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reports []Truncation
			if err := tc.run(tc.src, Options{OnTruncated: func(tr Truncation) { reports = append(reports, tr) }}); err != nil {
				t.Fatalf("refused: %v", err)
			}
			if tc.size == 0 {
				if len(reports) != 0 {
					t.Fatalf("a whole source reported %+v", reports)
				}
				return
			}
			if len(reports) != 1 {
				t.Fatalf("OnTruncated called %d times, want once", len(reports))
			}
			if r := reports[0]; r.FileSize != tc.size || r.ReachedMs <= 0 || (r.DurationMs > 0 && r.ReachedMs >= r.DurationMs) {
				t.Errorf("report %+v: want the cut file's size (%d) and a reach inside the declared duration", r, tc.size)
			}
		})
	}
}

// TestTruncatedMP4BeforeATracksFirstSample pins that a cut landing before a track's first sample fails the remux as
// a cut source, not as a codec to drop, and that SkipUnsupported still drops that track with the cut reported.
func TestTruncatedMP4BeforeATracksFirstSample(t *testing.T) {
	src := editShiftFixture(t, true)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	mv, err := parseMP4(f, int64(len(data)), sampleFull)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	last := int64(0) // the first sample of the track that starts last in the file
	for _, tr := range mv.tracks {
		last = max(last, tr.samples[0].offset)
	}
	cut := filepath.Join(t.TempDir(), "cut.mp4")
	if err := os.WriteFile(cut, data[:last+1], 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err = RemuxFromMP4(ctx, cut, filepath.Join(t.TempDir(), "out.mkv"))
	if !errors.Is(err, mkv.ErrSourceTruncated) || strings.Contains(err.Error(), "SkipUnsupported") {
		t.Fatalf("remux of an MP4 cut before a track's first sample: %v, want ErrSourceTruncated without the codec advice", err)
	}
	if err := RemuxToHLS(ctx, cut, t.TempDir()); !errors.Is(err, mkv.ErrSourceTruncated) {
		t.Errorf("HLS of an MP4 cut before a track's first sample: %v, want ErrSourceTruncated", err)
	}
	var reports []Truncation
	o := Options{SkipUnsupported: true, OnTruncated: func(tr Truncation) { reports = append(reports, tr) }}
	if err := RemuxFromMP4(ctx, cut, filepath.Join(t.TempDir(), "out.mkv"), o); err != nil {
		t.Fatalf("SkipUnsupported remux: %v", err)
	}
	if len(reports) != 1 {
		t.Errorf("the dropped track's cut reported %d times, want once", len(reports))
	}
}

// TestDeclaredSizeBoundedWalk pins DeclaredSize: a cut box declares past the file, a whole one the file itself, and
// a file of more top-level boxes than the walk reads (a fragmented one) declares nothing rather than being read whole.
func TestDeclaredSizeBoundedWalk(t *testing.T) {
	box := func(n int) []byte {
		b := make([]byte, n)
		b[2], b[3] = byte(n>>8), byte(n)
		copy(b[4:], "free")
		return b
	}
	var few, many []byte
	for i := 0; i < 3; i++ {
		few = append(few, box(16)...)
	}
	for i := 0; i < declaredSizeBoxes+4; i++ {
		many = append(many, box(16)...)
	}
	cases := []struct {
		name     string
		data     []byte
		declared int64
	}{
		{"whole", few, int64(len(few))},
		{"cut", few[:len(few)-4], int64(len(few))},
		{"many boxes, cut", many[:len(many)-4], 0},
	}
	for _, tc := range cases {
		p := filepath.Join(t.TempDir(), "f.mp4")
		if err := os.WriteFile(p, tc.data, 0o644); err != nil {
			t.Fatal(err)
		}
		size, declared, err := DeclaredSize(context.Background(), p)
		if err != nil || size != int64(len(tc.data)) || declared != tc.declared {
			t.Errorf("%s: DeclaredSize = %d, %d, %v; want %d, %d", tc.name, size, declared, err, len(tc.data), tc.declared)
		}
	}
}
