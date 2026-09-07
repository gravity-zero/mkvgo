package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// A Matroska file may declare any timebase it likes; all but a handful use the
// 1 ms default, which is why nothing here covered another one and why reading
// the Cues' raw units as milliseconds went unnoticed. nonDefaultScale is a real
// value seen in the wild - roughly 1/48000 s, so 48 timecode units to the
// millisecond, which turns every cue time 48x too large when it is not scaled.
const nonDefaultScale = 20832

// buildScaledFixture writes a file with the given timebase: one video keyframe
// per cluster, one audio block beside it, one cluster per second. Cues are left
// out - reindexing is what builds them.
func buildScaledFixture(t *testing.T, dir, name string, scale int64, clusters int) string {
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
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "test", WritingApp: "test"}}
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	if err := mw.WriteMetadata(c, tracks, int64(clusters)*1000); err != nil {
		t.Fatal(err)
	}
	for i := range clusters {
		ts := int64(i) * 1000
		blocks := []mkv.Block{
			// A length-prefixed NAL, so the keyframe extractor can actually pack
			// what it finds rather than fail on the payload and hide the timing
			// question this file exists to ask.
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0x00, 0x00, 0x00, 0x02, 0x09, 0x10}},
			{TrackNumber: 2, Timecode: ts, Keyframe: true, Data: []byte{0x01}},
		}
		if err := writer.WriteCluster(f, ts, scale, blocks); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReadCueTimesAreMillisecondsNotUnits is the root guard: the reader must
// hand back CuePoint.TimeMs in milliseconds whatever the file's timebase, on
// BOTH read paths. Before the fix the full read and the head-only read agreed
// with each other and were both 48x out.
func TestReadCueTimesAreMilliseconds(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := buildScaledFixture(t, dir, "src.mkv", nonDefaultScale, 5)
	dst := filepath.Join(dir, "out.mkv")
	if err := Reindex(ctx, src, dst); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	full, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	head, err := reader.OpenMeta(ctx, dst, reader.WithCues())
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Cues) != 5 || len(head.Cues) != 5 {
		t.Fatalf("cues: full %d, head-only %d, want 5 each", len(full.Cues), len(head.Cues))
	}
	for i := range full.Cues {
		// The clusters sit one second apart. A cue time is allowed to land a
		// couple of ms early - the ms-granularity round trip truncates twice -
		// but never 48x out.
		want := int64(i) * 1000
		if d := want - full.Cues[i].TimeMs; d < 0 || d > 2 {
			t.Errorf("full read cue %d: TimeMs = %d, want ~%d", i, full.Cues[i].TimeMs, want)
		}
		if full.Cues[i].TimeMs != head.Cues[i].TimeMs {
			t.Errorf("cue %d: full read says %d, head-only says %d - the two paths must agree",
				i, full.Cues[i].TimeMs, head.Cues[i].TimeMs)
		}
	}
	if full.Info.TimecodeScale != nonDefaultScale {
		t.Errorf("TimecodeScale = %d, want %d preserved verbatim", full.Info.TimecodeScale, nonDefaultScale)
	}
}

// TestReindexVerifiesOnNonDefaultScale is the ticket itself: Reindex rebuilt a
// CORRECT index and its own verification rejected it, comparing the walk's
// milliseconds against the reader's raw timecode units.
func TestReindexVerifiesOnNonDefaultScale(t *testing.T) {
	ctx := context.Background()
	for _, scale := range []int64{mkv.DefaultTimecodeScale, nonDefaultScale, 100_000, 10_000_000} {
		dir := t.TempDir()
		src := buildScaledFixture(t, dir, "src.mkv", scale, 6)
		dst := filepath.Join(dir, "out.mkv")
		if err := Reindex(ctx, src, dst); err != nil {
			t.Errorf("scale %d: reindex: %v", scale, err)
			continue
		}
		c, err := reader.Open(ctx, dst)
		if err != nil {
			t.Errorf("scale %d: reopen: %v", scale, err)
			continue
		}
		if len(c.Keyframes) != 6 {
			t.Errorf("scale %d: %d keyframes, want 6", scale, len(c.Keyframes))
		}
	}
}

// TestReindexInPlaceOnNonDefaultScale covers the repair-in-place path, which
// builds its cue points with the same walk and verifies them with the same
// comparison - so it failed the same way, and a guard on the copy path alone
// would not have caught it.
func TestReindexInPlaceOnNonDefaultScale(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := buildScaledFixture(t, dir, "inplace.mkv", nonDefaultScale, 6)

	if err := ReindexInPlace(ctx, path); err != nil {
		t.Fatalf("reindex in place: %v", err)
	}
	c, err := reader.OpenMeta(ctx, path, reader.WithCues())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Cues) != 6 {
		t.Fatalf("%d cues, want 6", len(c.Cues))
	}
	if last := c.Cues[len(c.Cues)-1].TimeMs; last > 5100 {
		t.Errorf("last cue at %dms in a 6s file - cue times were not scaled", last)
	}
}

// TestExtractKeyframeSampleOnNonDefaultScale guards the consumer with a visible
// output. ExtractKeyframeSample walks the cues for the one at or before atMs;
// comparing raw timecode units against a millisecond argument landed it on the
// cue at atMs/scale-ratio instead - on this timebase, 48 times too early, so a
// frame asked for 20s in came from 416ms in and every thumbnail of a feature
// film was drawn from its opening minutes.
func TestExtractKeyframeSampleOnNonDefaultScale(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := buildScaledFixture(t, dir, "src.mkv", nonDefaultScale, 30)
	dst := filepath.Join(dir, "out.mkv")
	if err := Reindex(ctx, src, dst); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	const atMs = 20_000
	ks, err := ExtractKeyframeSample(ctx, dst, atMs)
	if err != nil {
		t.Fatalf("extract at %dms: %v", atMs, err)
	}
	// Cues sit one second apart, so the keyframe at or before 20s is the 20s one
	// (a couple of ms early after the round trip).
	if ks.PtsMs < atMs-1100 || ks.PtsMs > atMs {
		t.Errorf("keyframe for %dms has PtsMs = %d, want within a second at or before it",
			atMs, ks.PtsMs)
	}
}

// TestRewriteOpsOnNonDefaultScale covers the ops that re-cluster their output.
// A SimpleBlock's offset from its cluster is a signed 16-bit count of TIMECODE
// UNITS, and the rewrite window was a flat 1000 milliseconds: on this timebase a
// second is 48003 units, so every one of these refused the file outright with
// "outside SimpleBlock's int16 range". The window now follows the timebase.
//
// This defect is older than the cue-time one and independent of it - it is a
// refusal, not a wrong answer, which is why nobody had hit it.
func TestRewriteOpsOnNonDefaultScale(t *testing.T) {
	ctx := context.Background()

	assertIntact := func(t *testing.T, path string, wantDurMs int64) {
		t.Helper()
		c, err := reader.Open(ctx, path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		if c.Info.TimecodeScale != nonDefaultScale {
			t.Errorf("TimecodeScale = %d, want %d preserved through the rewrite",
				c.Info.TimecodeScale, nonDefaultScale)
		}
		if c.DurationMs < wantDurMs-200 || c.DurationMs > wantDurMs+200 {
			t.Errorf("DurationMs = %d, want ~%d - the rewrite retimed the content",
				c.DurationMs, wantDurMs)
		}
	}

	t.Run("EditMetadata", func(t *testing.T) {
		dir := t.TempDir()
		src := buildScaledFixture(t, dir, "src.mkv", nonDefaultScale, 10)
		dst := filepath.Join(dir, "out.mkv")
		if err := EditMetadata(ctx, src, dst, func(c *mkv.Container) { c.Info.Title = "t" }); err != nil {
			t.Fatalf("edit metadata: %v", err)
		}
		assertIntact(t, dst, 10_000)
	})

	t.Run("RemoveTrack", func(t *testing.T) {
		dir := t.TempDir()
		src := buildScaledFixture(t, dir, "src.mkv", nonDefaultScale, 10)
		dst := filepath.Join(dir, "out.mkv")
		if err := RemoveTrack(ctx, src, dst, []uint64{2}); err != nil {
			t.Fatalf("remove track: %v", err)
		}
		assertIntact(t, dst, 10_000)
	})

	t.Run("Split", func(t *testing.T) {
		dir := t.TempDir()
		src0 := buildScaledFixture(t, dir, "src0.mkv", nonDefaultScale, 10)
		src := filepath.Join(dir, "src.mkv")
		if err := Reindex(ctx, src0, src); err != nil {
			t.Fatalf("reindex: %v", err)
		}
		parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: dir, EveryMs: 4000})
		if err != nil {
			t.Fatalf("split: %v", err)
		}
		if len(parts) < 2 {
			t.Fatalf("%d part(s) from a 10s file cut every 4s", len(parts))
		}
		for _, p := range parts {
			c, err := reader.Open(ctx, p)
			if err != nil {
				t.Fatalf("reopen %s: %v", filepath.Base(p), err)
			}
			if c.Info.TimecodeScale != nonDefaultScale {
				t.Errorf("%s: TimecodeScale = %d, want %d",
					filepath.Base(p), c.Info.TimecodeScale, nonDefaultScale)
			}
		}
	})
}

// TestRetimeAppliesToTheNearestUnit pins that a shift which is not an exact
// multiple of the file's timecode unit is APPLIED to the nearest one rather
// than refused. Refusing it never made the operation more precise - no file
// can express finer than its own resolution - it made it impossible: at this
// timebase only multiples of 651 ms are whole milliseconds, so an operator
// picking an offset from a slider was refused whatever they chose.
func TestRetimeAppliesToTheNearestUnit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := buildScaledFixture(t, dir, "src.mkv", nonDefaultScale, 6)

	// 100ms is 4800.7 units here: no exact answer exists in this file.
	const wantNs = 100_000_000
	if err := RetimeTracks(ctx, path, map[uint64]int64{2: wantNs}); err != nil {
		t.Fatalf("shift of %dns refused: %v", wantNs, err)
	}
	// It must have landed within half a unit of the request. Measured through
	// the audio-start delay rather than by re-reading blocks by hand: that is
	// the number a caller actually acts on.
	delays, err := AudioStartDelays(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	got := delays[2]
	if d := got - wantNs; d < -nonDefaultScale || d > nonDefaultScale {
		t.Errorf("audio now starts %dns after the video, want %dns within one timecode unit (%dns)",
			got, wantNs, nonDefaultScale)
	}
}

// TestRetimeRefusesAShiftThatRoundsToNothing pins the one refusal that stays,
// and that its message names the smallest shift the file CAN express - the
// caller cannot derive it without knowing the timebase.
func TestRetimeRefusesAShiftThatRoundsToNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := buildScaledFixture(t, dir, "src.mkv", nonDefaultScale, 6)

	err := RetimeTracks(ctx, path, map[uint64]int64{2: 1})
	if !errors.Is(err, ErrShiftNotRepresentable) {
		t.Fatalf("a 1ns shift must be refused as unrepresentable, got: %v", err)
	}
	if !strings.Contains(err.Error(), strconv.FormatInt(nonDefaultScale, 10)) {
		t.Errorf("refusal does not name the smallest expressible shift: %v", err)
	}
}

// TestClusterSpanMsFollowsTheTimebase pins the arithmetic itself: the cap is
// inert on the default timebase (a cluster could run 32s there, far past the 1s
// window) and binds only where a second no longer fits 16 bits.
func TestClusterSpanMsFollowsTheTimebase(t *testing.T) {
	cases := []struct{ scale, want int64 }{
		{mkv.DefaultTimecodeScale, defaultClusterDurationMs}, // 32767ms available, 1s window wins
		{10_000_000, defaultClusterDurationMs},               // coarser still
		{nonDefaultScale, 682},                               // 32767 * 20832 / 1e6
		{100_000, 3276},                                      // still above the window -> 1000
		{0, defaultClusterDurationMs},                        // unset: the default timebase
		{1, 0},                                               // degenerate: one cluster per block
	}
	for _, c := range cases {
		got := clusterSpanMs(c.scale)
		want := c.want
		if want > defaultClusterDurationMs {
			want = defaultClusterDurationMs
		}
		if got != want {
			t.Errorf("clusterSpanMs(%d) = %d, want %d", c.scale, got, want)
		}
		if units := got * mkv.DefaultTimecodeScale / max(c.scale, 1); c.scale > 0 && units > maxBlockRelTC {
			t.Errorf("clusterSpanMs(%d) = %dms = %d units, past the int16 limit", c.scale, got, units)
		}
	}
}

// TestCueHealthOnNonDefaultScale: a dense index (one cue per second) must be
// healthy whatever the timebase. Reading units as ms multiplied every gap by 48
// and reported a perfectly indexed file as sparse - which is what kept sending
// these files back for another repair.
func TestCueHealthOnNonDefaultScale(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := buildScaledFixture(t, dir, "src.mkv", nonDefaultScale, 120)
	dst := filepath.Join(dir, "out.mkv")
	if err := Reindex(ctx, src, dst); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	h, err := CueHealth(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Healthy {
		t.Errorf("dense index reported unhealthy: %s", h.Reason)
	}
	if len(h.Holes) != 0 {
		t.Errorf("holes = %d, want none in a one-cue-per-second index", len(h.Holes))
	}
	if h.LastCueMs > 120_000 {
		t.Errorf("LastCueMs = %d, beyond the file's own 120s - cue times were not scaled", h.LastCueMs)
	}

	issues, err := Validate(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range issues {
		if i.Code == "cues-stale" {
			t.Errorf("validate reported %q on a freshly rebuilt index: %s", i.Code, i.Message)
		}
	}

	d, err := Diagnose(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range d.Findings {
		t.Errorf("diagnose finding on a healthy file: %s - %s", f.Kind, f.Detail)
	}
	if d.TimecodeScale != nonDefaultScale {
		t.Errorf("Diagnosis.TimecodeScale = %d, want %d - a scan reads this to single "+
			"out the files an unusual timebase touches", d.TimecodeScale, nonDefaultScale)
	}
}
