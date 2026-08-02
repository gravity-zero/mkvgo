package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// An op whose output is not the same length as its input must SAY so in the
// Segment Info: the source's Duration is copied along with the rest of the
// metadata, and a player believes the header over the payload. Both ops below
// used to hand the writer a correct duration and have it silently overwritten
// by the copied one.

// TestJoin_DeclaresSummedDuration: the joined file declares the sum of its
// sources, not the length of the first one.
func TestJoin_DeclaresSummedDuration(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	tracks := []mkv.Track{videoTrack(1)}

	// Frames every second: the measured end of each source then lands on the
	// duration it declares, so the joined total is unambiguous.
	srcs := make([]string, 0, 3)
	for i, ms := range []int64{10000, 10000, 5000} {
		var sets [][]mkv.Block
		for tc := int64(0); tc < ms; tc += 1000 {
			sets = append(sets, []mkv.Block{{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: []byte("v")}})
		}
		srcs = append(srcs, buildMultiClusterMKV(t, dir, string(rune('a'+i))+".mkv", tracks, sets, ms))
	}

	dst := filepath.Join(dir, "joined.mkv")
	if err := Join(ctx, srcs, dst); err != nil {
		t.Fatal(err)
	}

	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if c.DurationMs != 25000 {
		t.Errorf("joined file declares %d ms, want 25000 (10000+10000+5000); "+
			"10000 means the first source's Duration overwrote the summed one", c.DurationMs)
	}
	// The declaration is what was MEASURED at the last seam, not the sum of what
	// the sources declared: keyframe-cut parts hold a little past their nominal
	// end, and a film rejoined from twelve of them fell 24.8 s short.
	rep, err := Analyze(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if rep.DeclaredDurationMs < rep.DurationMs {
		t.Errorf("declares %d ms but holds %d ms - the seek bar stops %d ms early",
			rep.DeclaredDurationMs, rep.DurationMs, rep.DurationMs-rep.DeclaredDurationMs)
	}
}

// TestAddTrack_DeclaresExtendedDuration: adding a track that outlasts the
// source makes the file longer, so the output must stop declaring the source's
// length. AddTrack already computed the extended value; it was being dropped.
func TestAddTrack_DeclaresExtendedDuration(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	src := buildMinimalMKV(t, dir, "video.mkv", []mkv.Track{videoTrack(1)}, []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")},
		{TrackNumber: 1, Timecode: 3000, Keyframe: true, Data: []byte("v")},
	}, 4000)
	// A commentary track that runs 5 s past the end of the video.
	add := buildMinimalMKV(t, dir, "commentary.mkv", []mkv.Track{audioTrack(1)}, []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("a")},
		{TrackNumber: 1, Timecode: 8000, Keyframe: true, Data: []byte("a")},
	}, 9000)

	dst := filepath.Join(dir, "out.mkv")
	if err := AddTrack(ctx, src, dst, mkv.TrackInput{SourcePath: add, TrackID: 1}); err != nil {
		t.Fatal(err)
	}
	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if c.DurationMs != 9000 {
		t.Errorf("file with the added track declares %d ms, want 9000; 4000 means the source's "+
			"Duration outlived the track that extended the file", c.DurationMs)
	}
}

// TestSalvage_KeepsDeclaredDurationAndAnalyzeSeesIt pins a DELIBERATE choice
// rather than a fix: a salvaged file keeps the byte-for-byte Segment Info of
// its damaged source, so a truncated download still announces the full length.
// Re-encoding the Duration there would shift every offset the rollback delta
// records. What must hold is that the discrepancy is not invisible - Analyze
// measures it - and that restating it stays one EditInPlace away.
func TestSalvage_KeepsDeclaredDurationAndAnalyzeSeesIt(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	src := salvageFixture(t, dir, "full.mkv", 20) // 20 clusters, declares 20000 ms
	data := readAll(t, src)
	offsets := findAll(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	if len(offsets) < 10 {
		t.Fatalf("fixture has %d clusters, need >= 10", len(offsets))
	}
	truncated := filepath.Join(dir, "cut.mkv")
	writeAll(t, truncated, data[:offsets[8]+15]) // cut inside the 9th cluster

	dst := filepath.Join(dir, "salvaged.mkv")
	report, err := Salvage(ctx, truncated, dst)
	if err != nil {
		t.Fatal(err)
	}
	if !report.TruncatedTail {
		t.Fatalf("fixture did not produce a truncated tail: %+v", report.DamagedRanges)
	}

	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if c.DurationMs != 20000 {
		t.Errorf("salvaged file declares %d ms, want the source's 20000 (verbatim Info is the "+
			"contract the rollback delta rests on)", c.DurationMs)
	}

	// The gap is measurable, and flagged.
	rep, err := Analyze(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if rep.DurationMs >= rep.DeclaredDurationMs {
		t.Errorf("Analyze: walked %d ms vs declared %d ms - the recovered file should measure SHORTER",
			rep.DurationMs, rep.DeclaredDurationMs)
	}
	var warned bool
	for _, w := range rep.Warnings {
		if strings.Contains(w, "declared duration") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("Analyze did not warn about the duration mismatch: %v", rep.Warnings)
	}

	// And the documented remedy works end to end.
	if err := EditInPlace(ctx, dst, func(c *mkv.Container) {
		c.Info.Duration = float64(rep.DurationMs)
	}); err != nil {
		t.Fatal(err)
	}
	if c, err := reader.Open(ctx, dst); err != nil {
		t.Fatal(err)
	} else if c.DurationMs != rep.DurationMs {
		t.Errorf("after restating: declares %d ms, want %d", c.DurationMs, rep.DurationMs)
	}
}

// TestEditDuration_SyncsDurationMs: an edit callback restates a length through
// Info.Duration, the raw stored field. The container must not then report two
// different lengths depending on which field the caller reads.
func TestEditDuration_SyncsDurationMs(t *testing.T) {
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, Duration: 9000}, DurationMs: 5000}
	syncDurationMs(c)
	if c.DurationMs != 9000 {
		t.Errorf("DurationMs = %d after restating Info.Duration = 9000, want 9000", c.DurationMs)
	}
	// A TimecodeScale of 500 µs halves the millisecond value of the same raw field.
	c = &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 500_000, Duration: 9000}, DurationMs: 5000}
	syncDurationMs(c)
	if c.DurationMs != 4500 {
		t.Errorf("DurationMs = %d with TimecodeScale 500_000, want 4500", c.DurationMs)
	}
	// Cleared Duration means "derive from DurationMs": the existing value stands.
	c = &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000}, DurationMs: 5000}
	syncDurationMs(c)
	if c.DurationMs != 5000 {
		t.Errorf("DurationMs = %d after clearing Info.Duration, want it untouched at 5000", c.DurationMs)
	}
}

// TestEditDuration_ExplicitEditWins is the guard on the other side of the same
// precedence: an edit callback that sets Info.Duration is the ONLY way a caller
// can restate a file's declared length, and both edit ops must honour it. A
// "fix" that let the ops' duration parameter win unconditionally would silently
// discard this - the failure mode is invisible, so it is pinned here.
func TestEditDuration_ExplicitEditWins(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	blocks := []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")}}

	// TimecodeScale is 1 ms here, so Info.Duration is expressed in whole ms.
	const want = 12345
	restate := func(c *mkv.Container) { c.Info.Duration = want }

	src := buildMinimalMKV(t, dir, "a.mkv", []mkv.Track{videoTrack(1)}, blocks, 5000)
	dst := filepath.Join(dir, "edited.mkv")
	if err := EditMetadata(ctx, src, dst, restate); err != nil {
		t.Fatal(err)
	}
	if c, err := reader.Open(ctx, dst); err != nil {
		t.Fatal(err)
	} else if c.DurationMs != want {
		t.Errorf("EditMetadata: declares %d ms, want %d - the explicit edit was dropped", c.DurationMs, want)
	}

	// The in-place path is the one a caller uses to restamp a duration without
	// rewriting the clusters.
	inPlace := buildMinimalMKV(t, dir, "b.mkv", []mkv.Track{videoTrack(1)}, blocks, 5000)
	if err := EditInPlace(ctx, inPlace, restate); err != nil {
		t.Fatal(err)
	}
	if c, err := reader.Open(ctx, inPlace); err != nil {
		t.Fatal(err)
	} else if c.DurationMs != want {
		t.Errorf("EditInPlace: declares %d ms, want %d - the explicit edit was dropped", c.DurationMs, want)
	}
}

// TestSplit_DeclaresPerSegmentDuration: each part declares its own range, not
// the whole source. This is the one that is easy to miss and the most visible -
// every part of the split is wrong, not just one file.
func TestSplit_DeclaresPerSegmentDuration(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	src := filepath.Join(dir, "src.mkv")

	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(c, []mkv.Track{videoTrack(1)}, 6000))
	for tc := int64(0); tc < 6000; tc += 500 {
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{
			{TrackNumber: 1, Timecode: tc, Keyframe: tc%2000 == 0, Data: []byte("v")},
		}))
	}
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())

	// Two ranges, one closed and one open-ended (EndMs == 0 → source end), so
	// both branches of the per-range duration are covered.
	parts, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src, OutputDir: filepath.Join(dir, "seg"),
		Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 2000}, {StartMs: 2000, EndMs: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %v, want 2", parts)
	}
	for i, want := range []int64{2000, 4000} {
		got, err := reader.Open(ctx, parts[i])
		if err != nil {
			t.Fatal(err)
		}
		if got.DurationMs != want {
			t.Errorf("part %d declares %d ms, want %d; 6000 means every segment claims the whole source",
				i+1, got.DurationMs, want)
		}
	}
}

// TestJoin_DeclaresWhatItHoldsAfterASplit is the case the arithmetic alone
// cannot reach: parts cut on keyframes each hold a little past the range they
// were asked for, so the sum of what they DECLARE falls short of what the
// joined file actually contains. Declared from that sum, the seek bar stops
// before the end - 24.8 s early on a twelve-part split of a real film.
func TestJoin_DeclaresWhatItHoldsAfterASplit(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	// Keyframes every 2 s, frames every 100 ms: a cut asked for at 5 s lands on
	// the keyframe at 6 s, and the part keeps everything up to it.
	var sets [][]mkv.Block
	for base := int64(0); base < 12000; base += 1000 {
		var cluster []mkv.Block
		for tc := base; tc < base+1000; tc += 100 {
			cluster = append(cluster, mkv.Block{TrackNumber: 1, Timecode: tc,
				Keyframe: tc%2000 == 0, Data: []byte("v")})
		}
		sets = append(sets, cluster)
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, sets, 12000)

	parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "p"),
		Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 5000}, {StartMs: 5000, EndMs: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	joined := filepath.Join(dir, "joined.mkv")
	if err := Join(ctx, parts, joined); err != nil {
		t.Fatal(err)
	}

	rep, err := Analyze(ctx, joined)
	if err != nil {
		t.Fatal(err)
	}
	if rep.DeclaredDurationMs < rep.DurationMs {
		t.Errorf("declares %d ms but holds %d ms: the seek bar stops %d ms early - the sum of the "+
			"parts' declared durations was used instead of the measured end",
			rep.DeclaredDurationMs, rep.DurationMs, rep.DurationMs-rep.DeclaredDurationMs)
	}
}
