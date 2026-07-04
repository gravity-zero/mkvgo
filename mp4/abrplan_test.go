package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlanABRMatchesFullPass proves the on-demand ABR plan serves every media
// resource byte-identically to RemuxToABR: same v{k}/ init segments, media
// segments, media playlists and subtitle renditions. (The master's BANDWIDTH is
// estimated on Matroska plans, so it is checked for structure, not bytes -
// the single-source PlanHLS parity convention.)
func TestPlanABRMatchesFullPass(t *testing.T) {
	src, _ := buildLacedFixture(t) // video + laced audio + Cues
	sources := []string{src, src}  // two variants: v1 complete, v2 video-only
	dir := t.TempDir()
	if err := RemuxToABR(context.Background(), sources, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanABR(context.Background(), sources, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if plan.NumVariants() != 2 {
		t.Fatalf("variants = %d, want 2", plan.NumVariants())
	}

	checked := 0
	for _, name := range plan.Resources() {
		if name == "master.m3u8" || strings.HasSuffix(name, "/manifest.mpd") {
			continue // estimated BANDWIDTH on Matroska plans
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
	// The master must declare both variants and share the reference audio group.
	if n := bytes.Count(plan.MasterPlaylist(), []byte("EXT-X-STREAM-INF")); n != 2 {
		t.Errorf("master has %d variants, want 2", n)
	}
	if !bytes.Contains(plan.MasterPlaylist(), []byte("TYPE=AUDIO")) {
		t.Error("master is missing the shared audio group")
	}
}

// A single-source ABR is rejected (use PlanHLS); a v{k} out of range errors.
func TestPlanABRRejectsSingleAndBadResource(t *testing.T) {
	src, _ := buildLacedFixture(t)
	if _, err := PlanABR(context.Background(), []string{src}, Options{SegmentMs: 2000}); err == nil {
		t.Error("PlanABR with one source should error")
	}
	plan, err := PlanABR(context.Background(), []string{src, src}, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := plan.Resource(context.Background(), "v9/init.mp4"); err == nil {
		t.Error("out-of-range variant should error")
	}
	if _, _, err := plan.Resource(context.Background(), "init.mp4"); err == nil {
		t.Error("a bare (non-v{k}) resource should error on the ABR plan")
	}
}
