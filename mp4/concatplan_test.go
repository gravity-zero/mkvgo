package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlanConcatMatchesFullPass proves the on-demand concat plan serves every
// media resource byte-identically to RemuxConcatToHLS: the concatenated
// playlists, every part's init/segments, and the shifted subtitle renditions.
func TestPlanConcatMatchesFullPass(t *testing.T) {
	src0 := buildConcatSource(t, 4000, "eng", "part0 cue", 500)
	src1 := buildConcatSource(t, 2000, "eng", "part1 cue", 300)
	src2 := buildConcatSource(t, 3000, "eng", "part2 cue", 700)
	sources := []string{src0, src1, src2}
	dir := t.TempDir()
	if err := RemuxConcatToHLS(context.Background(), sources, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanConcat(context.Background(), sources, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if plan.NumParts() != 3 {
		t.Fatalf("NumParts = %d, want 3", plan.NumParts())
	}

	checked := 0
	for _, name := range plan.Resources() {
		if name == "master.m3u8" {
			continue // estimated BANDWIDTH on Matroska plans (the PlanHLS convention)
		}
		got, _, err := plan.Resource(context.Background(), name)
		if err != nil {
			t.Errorf("Resource(%q): %v", name, err)
			continue
		}
		want, ferr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if ferr != nil {
			t.Errorf("full pass did not write %s: %v", name, ferr)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the full pass (%d vs %d bytes)", name, len(got), len(want))
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no resources compared")
	}
}

// The plan's structural checks mirror the full pass: one variant, VERSION 6
// media playlists, DISCONTINUITY at each of the two part boundaries.
func TestPlanConcatStructure(t *testing.T) {
	src0 := buildConcatSource(t, 2000, "eng", "a", 100)
	src1 := buildConcatSource(t, 1000, "eng", "b", 100)
	plan, err := PlanConcat(context.Background(), []string{src0, src1}, Options{SegmentMs: 500})
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(plan.MasterPlaylist(), []byte("#EXT-X-STREAM-INF:")); n != 1 {
		t.Errorf("master must declare exactly one variant, got %d", n)
	}
	pl, _, err := plan.Resource(context.Background(), "playlist.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pl, []byte("#EXT-X-VERSION:6")) {
		t.Errorf("concatenated media playlist must be VERSION 6:\n%s", pl)
	}
	if n := bytes.Count(pl, []byte("#EXT-X-DISCONTINUITY")); n != 1 {
		t.Errorf("two parts must produce exactly 1 DISCONTINUITY, got %d", n)
	}
}

// A single source is rejected (use PlanHLS); an out-of-range part errors.
func TestPlanConcatRejectsSingleAndBadResource(t *testing.T) {
	src := buildConcatSource(t, 2000, "eng", "a", 100)
	if _, err := PlanConcat(context.Background(), []string{src}, Options{SegmentMs: 500}); err == nil {
		t.Error("PlanConcat with one source should error")
	}
	plan, err := PlanConcat(context.Background(), []string{src, src}, Options{SegmentMs: 500})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := plan.Resource(context.Background(), "p9/init.mp4"); err == nil {
		t.Error("out-of-range part should error")
	}
	if _, _, err := plan.Resource(context.Background(), "init.mp4"); err == nil {
		t.Error("a bare (non-p{k}) resource should error on the concat plan")
	}
	if _, _, err := plan.Resource(context.Background(), "manifest.mpd"); err == nil {
		t.Error("concat v1 emits no combined DASH manifest")
	}
}

// Incompatible sources and unsupported options are refused before any source
// is planned.
func TestPlanConcatRefusals(t *testing.T) {
	src0 := buildConcatSource(t, 2000, "eng", "a", 100)
	src1 := buildConcatSourceAV1(t, 1000)
	if _, err := PlanConcat(context.Background(), []string{src0, src1}, Options{SegmentMs: 500}); err == nil {
		t.Error("incompatible video codecs should be refused")
	} else if !strings.Contains(err.Error(), "not compatible") {
		t.Errorf("error should explain the incompatibility: %v", err)
	}
	if _, err := PlanConcat(context.Background(), []string{src0, src0}, Options{Encrypt: &HLSEncryption{Key: make([]byte, 16)}}); err == nil {
		t.Error("Encrypt should be refused")
	}
	if _, err := PlanConcat(context.Background(), []string{src0, src0}, Options{SingleFile: true}); err == nil {
		t.Error("SingleFile should be refused")
	}
}

// Mismatched subtitle layouts drop subtitles from the plan (reported via
// OnDrop) rather than failing the whole plan.
func TestPlanConcatMismatchedSubsDropped(t *testing.T) {
	src0 := buildConcatSource(t, 2000, "eng", "a", 100)
	src1 := buildConcatSourceNoSubs(t, 1000)
	var dropped []DroppedTrack
	plan, err := PlanConcat(context.Background(), []string{src0, src1}, Options{SegmentMs: 500,
		OnDrop: func(d DroppedTrack) { dropped = append(dropped, d) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) == 0 || !strings.Contains(dropped[len(dropped)-1].Reason, "subtitle") {
		t.Fatalf("expected an OnDrop reason mentioning subtitles, got %+v", dropped)
	}
	for _, name := range plan.Resources() {
		if strings.HasPrefix(name, "sub") {
			t.Errorf("no subtitle resource should be listed, found %q", name)
		}
	}
	if _, _, err := plan.Resource(context.Background(), "sub1.m3u8"); err == nil {
		t.Error("sub1.m3u8 should error when subtitle layouts mismatch")
	}
}
