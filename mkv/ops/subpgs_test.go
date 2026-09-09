package ops

import (
	"context"
	"image"
	"os"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// --- synthetic PGS display sets ------------------------------------------
//
// Built from literal segment bytes on purpose: these tests must fail if the
// decoder's idea of the wire format drifts, so they do not share the builders
// used inside the subtitle package.

func pgsSeg(typ byte, payload []byte) []byte {
	return append([]byte{typ, byte(len(payload) >> 8), byte(len(payload))}, payload...)
}

// pgsShow is one display set that puts a 4x2 picture at (x,y) on a 1920x1080
// plane. epoch starts a new epoch, which also (re)defines the palette.
func pgsShow(objID uint16, x, y int, epoch bool) []byte {
	state := byte(0)
	if epoch {
		state = 0x80
	}
	pcs := pgsSeg(0x16, []byte{
		0x07, 0x80, 0x04, 0x38, // 1920x1080
		0x10, 0x00, 0x00, state, 0x00, 0x00, 0x01,
		byte(objID >> 8), byte(objID), 0x00, 0x00,
		byte(x >> 8), byte(x), byte(y >> 8), byte(y),
	})
	out := append([]byte{}, pcs...)
	out = append(out, pgsSeg(0x17, []byte{0x01, 0x00,
		byte(x >> 8), byte(x), byte(y >> 8), byte(y), 0x00, 0x04, 0x00, 0x02})...)
	if epoch {
		out = append(out, pgsSeg(0x14, []byte{
			0x00, 0x00,
			0x01, 235, 128, 128, 255, // opaque white
		})...)
	}
	// 4x2: the first line all colour 1, the second line untouched.
	rle := []byte{0x00, 0x84, 0x01, 0x00, 0x00, 0x00, 0x00}
	ods := []byte{byte(objID >> 8), byte(objID), 0x00, 0xc0,
		0x00, 0x00, byte(len(rle) + 4), 0x00, 0x04, 0x00, 0x02}
	out = append(out, pgsSeg(0x15, append(ods, rle...))...)
	return append(out, pgsSeg(0x80, nil)...)
}

// pgsClear is the empty composition that takes the picture off the screen.
func pgsClear() []byte {
	pcs := pgsSeg(0x16, []byte{0x07, 0x80, 0x04, 0x38, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	return append(pcs, pgsSeg(0x80, nil)...)
}

// buildPGSFixture writes a source whose bulk is a video track, so a test can
// tell an index-served extraction (which seeks) from a walk. Track 2 is PGS,
// track 3 is text - the codec the PGS extractor must refuse.
func buildPGSFixture(t *testing.T, dir string) string {
	t.Helper()
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "und"},
		subtitleTrack(2, "pgs"),
		subtitleTrack(3, "srt"),
	}
	payload := make([]byte, 512<<10)
	var blocks []mkv.Block
	add := func(ts int64, dur int64, data []byte) {
		blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: ts, Duration: dur, Data: data})
	}
	for i := 0; i < 8; i++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: int64(i * 250), Keyframe: true, Data: payload})
	}
	add(0, 0, pgsShow(10, 100, 900, true)) // ends on the clear at 500
	add(500, 0, pgsClear())
	add(1000, 400, pgsShow(11, 200, 950, false)) // ends on its own BlockDuration
	add(2000, 0, pgsShow(12, 300, 960, false))   // never closed: the default applies
	blocks = append(blocks, mkv.Block{TrackNumber: 3, Timecode: 0, Duration: 500, Data: []byte("hello")})
	return buildMinimalMKV(t, dir, "pgs.mkv", tracks, blocks, 3000)
}

func wantPGSCues() []PGSCue {
	return []PGSCue{
		{StartMs: 0, EndMs: 500, X: 100, Y: 900, ScreenW: 1920, ScreenH: 1080},
		{StartMs: 1000, EndMs: 1400, X: 200, Y: 950, ScreenW: 1920, ScreenH: 1080},
		{StartMs: 2000, EndMs: 2000 + defaultSubDurationMs, X: 300, Y: 960, ScreenW: 1920, ScreenH: 1080},
	}
}

func checkPGSCues(t *testing.T, got []PGSCue, what string) {
	t.Helper()
	want := wantPGSCues()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d cues, want %d", what, len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.StartMs != w.StartMs || g.EndMs != w.EndMs {
			t.Errorf("%s: cue %d spans %d..%d ms, want %d..%d", what, i, g.StartMs, g.EndMs, w.StartMs, w.EndMs)
		}
		if g.X != w.X || g.Y != w.Y {
			t.Errorf("%s: cue %d at (%d,%d), want (%d,%d)", what, i, g.X, g.Y, w.X, w.Y)
		}
		if g.ScreenW != w.ScreenW || g.ScreenH != w.ScreenH {
			t.Errorf("%s: cue %d screen %dx%d, want %dx%d", what, i, g.ScreenW, g.ScreenH, w.ScreenW, w.ScreenH)
		}
		if g.Image == nil || g.Image.Bounds() != image.Rect(0, 0, 4, 2) {
			t.Fatalf("%s: cue %d image = %v, want a 4x2 picture", what, i, g.Image)
		}
		off := g.Image.PixOffset(0, 0)
		if got := [4]uint8(g.Image.Pix[off : off+4]); got != [4]uint8{255, 255, 255, 255} {
			t.Errorf("%s: cue %d top-left pixel = %v, want opaque white", what, i, got)
		}
	}
}

// The walking extractor is the reference: cue times, positions and pixels.
func TestExtractSubtitlePGS_Walk(t *testing.T) {
	path := buildPGSFixture(t, t.TempDir())
	cues, err := ExtractSubtitlePGS(context.Background(), path, 2)
	if err != nil {
		t.Fatalf("ExtractSubtitlePGS: %v", err)
	}
	checkPGSCues(t, cues, "walk")
}

// The point of the ticket: a PGS track that is already in a SubtitleIndex - one
// built by an earlier release, since resolveIndexTracks never filtered on codec -
// extracts by seeking, with no rebuild and no change to the index format.
func TestExtractSubtitlePGSFrom_ServedFromAnExistingIndex(t *testing.T) {
	path := buildPGSFixture(t, t.TempDir())
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	ix, err := BuildSubtitleIndex(context.Background(), path, nil)
	if err != nil {
		t.Fatalf("BuildSubtitleIndex: %v", err)
	}
	if n := ix.Blocks(2); n != 4 {
		t.Fatalf("the PGS track has %d indexed blocks, want 4", n)
	}
	// Round-trip through the wire format: the index a caller has cached is a
	// decoded one, and it must serve bitmaps just as well.
	blob, err := ix.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var cached SubtitleIndex
	if err := cached.UnmarshalBinary(blob); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}

	cfs := &countingOpenFS{}
	cues, err := ExtractSubtitlePGSFrom(context.Background(), path, 2, &cached, mkv.Options{FS: cfs.fs()})
	if err != nil {
		t.Fatalf("ExtractSubtitlePGSFrom: %v", err)
	}
	checkPGSCues(t, cues, "served")

	// Serving must seek, not walk: the block reads are a small fraction of the
	// file. The last handle is the block walk (the first is the metadata pass).
	walk := cfs.reads[len(cfs.reads)-1]
	if limit := st.Size() / 10; walk.n > limit {
		t.Errorf("serving from the index read %d of %d bytes - it must seek to the blocks, not walk",
			walk.n, st.Size())
	}
}

// The streaming form must produce exactly what the slice form does; it is the
// one a caller with a feature-length track has to use.
func TestForEachSubtitlePGS_MatchesSliceForm(t *testing.T) {
	path := buildPGSFixture(t, t.TempDir())
	ix, err := BuildSubtitleIndex(context.Background(), path, []uint64{2})
	if err != nil {
		t.Fatal(err)
	}
	var streamed []PGSCue
	live := 0
	if err := ForEachSubtitlePGSFrom(context.Background(), path, 2, ix, func(c PGSCue) error {
		live++
		streamed = append(streamed, c)
		return nil
	}); err != nil {
		t.Fatalf("ForEachSubtitlePGSFrom: %v", err)
	}
	checkPGSCues(t, streamed, "streamed")

	var walked []PGSCue
	if err := ForEachSubtitlePGS(context.Background(), path, 2, func(c PGSCue) error {
		walked = append(walked, c)
		return nil
	}); err != nil {
		t.Fatalf("ForEachSubtitlePGS: %v", err)
	}
	checkPGSCues(t, walked, "walked")
}

// An error from the callback stops the extraction and reaches the caller
// unwrapped, so a consumer writing PNGs can abort on a full disk.
func TestForEachSubtitlePGS_CallbackErrorStops(t *testing.T) {
	path := buildPGSFixture(t, t.TempDir())
	sentinel := os.ErrPermission
	seen := 0
	err := ForEachSubtitlePGS(context.Background(), path, 2, func(PGSCue) error {
		seen++
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("got %v, want the callback's own error", err)
	}
	if seen != 1 {
		t.Errorf("the callback ran %d times after returning an error, want 1", seen)
	}
}

// The refusals are Evey's operator-facing text: each must say what the track
// actually is, so the next move is obvious.
func TestExtractSubtitlePGS_Refusals(t *testing.T) {
	path := buildPGSFixture(t, t.TempDir())
	ix, err := BuildSubtitleIndex(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		trackID uint64
		want    string
	}{
		{"a text track", 3, "extract it as WebVTT instead"},
		{"a video track", 1, "not found"},
		{"no such track", 9, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ExtractSubtitlePGS(context.Background(), path, tc.trackID)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("walking: got %v, want an error mentioning %q", err, tc.want)
			}
			_, err = ExtractSubtitlePGSFrom(context.Background(), path, tc.trackID, ix)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("served: got %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// An index built for another file must never be used to seek into this one.
func TestExtractSubtitlePGSFrom_StaleIndexRefused(t *testing.T) {
	dir := t.TempDir()
	path := buildPGSFixture(t, dir)
	ix, err := BuildSubtitleIndex(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractSubtitlePGSFrom(context.Background(), path, 2, nil); err == nil {
		t.Error("a nil index was accepted")
	}
	ix.fileSize++
	if _, err := ExtractSubtitlePGSFrom(context.Background(), path, 2, ix); err == nil {
		t.Error("an index built for a different size was accepted")
	}
}

// Two objects in one display set (a disc splitting a line across windows) become
// one picture on the smallest rectangle covering both, so X and Y stay a real
// screen position.
func TestExtractSubtitlePGS_TwoObjectsAreComposited(t *testing.T) {
	dir := t.TempDir()
	pcs := pgsSeg(0x16, []byte{
		0x07, 0x80, 0x04, 0x38,
		0x10, 0x00, 0x00, 0x80, 0x00, 0x00, 0x02,
		0x00, 0x0a, 0x00, 0x00, 0x00, 0x64, 0x03, 0x84, // object 10 at (100,900)
		0x00, 0x0b, 0x00, 0x00, 0x00, 0x6a, 0x03, 0x8a, // object 11 at (106,906)
	})
	pds := pgsSeg(0x14, []byte{0x00, 0x00, 0x01, 235, 128, 128, 255})
	rle := []byte{0x00, 0x84, 0x01, 0x00, 0x00, 0x00, 0x00}
	ods := func(id byte) []byte {
		return pgsSeg(0x15, append([]byte{0x00, id, 0x00, 0xc0,
			0x00, 0x00, byte(len(rle) + 4), 0x00, 0x04, 0x00, 0x02}, rle...))
	}
	block := append(append(append(append([]byte{}, pcs...), pds...), ods(0x0a)...), ods(0x0b)...)
	block = append(block, pgsSeg(0x80, nil)...)

	path := buildMinimalMKV(t, dir, "two.mkv",
		[]mkv.Track{subtitleTrack(1, "pgs")},
		[]mkv.Block{{TrackNumber: 1, Timecode: 0, Duration: 100, Data: block}}, 100)

	cues, err := ExtractSubtitlePGS(context.Background(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1", len(cues))
	}
	c := cues[0]
	if c.X != 100 || c.Y != 900 {
		t.Errorf("composited at (%d,%d), want the union origin (100,900)", c.X, c.Y)
	}
	// (100,900)+4x2 and (106,906)+4x2 span 100..110 by 900..908.
	if got := c.Image.Bounds(); got != image.Rect(0, 0, 10, 8) {
		t.Errorf("composited bounds %v, want 10x8", got)
	}
	for _, p := range []image.Point{{X: 0, Y: 0}, {X: 6, Y: 6}} {
		off := c.Image.PixOffset(p.X, p.Y)
		if got := [4]uint8(c.Image.Pix[off : off+4]); got != [4]uint8{255, 255, 255, 255} {
			t.Errorf("pixel %v = %v, want opaque white - an object was lost in the composite", p, got)
		}
	}
}

// pgsShowN builds one display set placing the same 4x2 picture at several
// positions, so the ops-level compositing is exercised in shapes no real disc
// has yet produced.
func pgsShowN(at [][2]int) []byte {
	head := []byte{0x07, 0x80, 0x04, 0x38, 0x10, 0x00, 0x00, 0x80, 0x00, 0x00, byte(len(at))}
	for i, p := range at {
		head = append(head,
			0x00, byte(0x0a+i), 0x00, 0x00,
			byte(p[0]>>8), byte(p[0]), byte(p[1]>>8), byte(p[1]))
	}
	out := pgsSeg(0x16, head)
	out = append(out, pgsSeg(0x14, []byte{0x00, 0x00, 0x01, 235, 128, 128, 255})...)
	rle := []byte{0x00, 0x84, 0x01, 0x00, 0x00, 0x00, 0x00}
	for i := range at {
		ods := []byte{0x00, byte(0x0a + i), 0x00, 0xc0,
			0x00, 0x00, byte(len(rle) + 4), 0x00, 0x04, 0x00, 0x02}
		out = append(out, pgsSeg(0x15, append(ods, rle...))...)
	}
	return append(out, pgsSeg(0x80, nil)...)
}

// The union rectangle must cover every object exactly, whatever their layout -
// this is the path that turns several objects into the single picture a PGSCue
// carries, and no disc in the corpus exercises it.
func TestFlattenPGS_UnionGeometry(t *testing.T) {
	cases := []struct {
		name   string
		at     [][2]int
		wantXY [2]int
		wantWH [2]int
	}{
		{"side by side", [][2]int{{100, 900}, {200, 900}}, [2]int{100, 900}, [2]int{104, 2}},
		{"stacked", [][2]int{{100, 60}, {100, 900}}, [2]int{100, 60}, [2]int{4, 842}},
		{"reversed order still yields the top-left origin",
			[][2]int{{500, 900}, {100, 60}}, [2]int{100, 60}, [2]int{404, 842}},
		{"overlapping", [][2]int{{100, 100}, {102, 101}}, [2]int{100, 100}, [2]int{6, 3}},
		{"three objects", [][2]int{{10, 10}, {50, 10}, {30, 200}}, [2]int{10, 10}, [2]int{44, 192}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := buildMinimalMKV(t, dir, "multi.mkv",
				[]mkv.Track{subtitleTrack(1, "pgs")},
				[]mkv.Block{{TrackNumber: 1, Timecode: 0, Duration: 100, Data: pgsShowN(tc.at)}}, 100)

			cues, err := ExtractSubtitlePGS(context.Background(), path, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(cues) != 1 {
				t.Fatalf("got %d cues, want 1", len(cues))
			}
			c := cues[0]
			if c.X != tc.wantXY[0] || c.Y != tc.wantXY[1] {
				t.Errorf("origin (%d,%d), want (%d,%d)", c.X, c.Y, tc.wantXY[0], tc.wantXY[1])
			}
			if got := c.Image.Bounds(); got.Dx() != tc.wantWH[0] || got.Dy() != tc.wantWH[1] {
				t.Errorf("size %dx%d, want %dx%d", got.Dx(), got.Dy(), tc.wantWH[0], tc.wantWH[1])
			}
			// Every object must have landed: its top-left pixel is opaque white.
			for _, p := range tc.at {
				x, y := p[0]-c.X, p[1]-c.Y
				off := c.Image.PixOffset(x, y)
				if got := [4]uint8(c.Image.Pix[off : off+4]); got != [4]uint8{255, 255, 255, 255} {
					t.Errorf("object at screen (%d,%d) missing from the composite: pixel = %v", p[0], p[1], got)
				}
			}
		})
	}
}

// Forced is per cue but a composite has several objects: any forced object must
// make the cue forced, or a disc that marks only the sign in a mixed set would
// lose it.
func TestFlattenPGS_ForcedSurvivesCompositing(t *testing.T) {
	dir := t.TempDir()
	block := pgsShowN([][2]int{{10, 10}, {50, 50}})
	// Set the forced flag on the SECOND composition object only.
	block[3+11+8+3] |= 0x40
	path := buildMinimalMKV(t, dir, "forced.mkv",
		[]mkv.Track{subtitleTrack(1, "pgs")},
		[]mkv.Block{{TrackNumber: 1, Timecode: 0, Duration: 100, Data: block}}, 100)

	cues, err := ExtractSubtitlePGS(context.Background(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1", len(cues))
	}
	if !cues[0].Forced {
		t.Error("a composite holding one forced object did not report Forced")
	}
}
