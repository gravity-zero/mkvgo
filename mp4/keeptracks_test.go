package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestKeepTracksDropsRenditions is the Virtual Edit Layer: from one source with
// two audio tracks, KeepTracks serves a version with only the chosen one - no
// copy, the dropped rendition is simply not packaged. Keeping no video errors.
func TestKeepTracksDropsRenditions(t *testing.T) {
	src := "../internal/testdata/regfix.mkv" // h264 + 2×aac
	c, err := reader.OpenWithFS(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	var vid uint64
	var auds []uint64
	for _, tr := range c.Tracks {
		switch tr.Type {
		case mkv.VideoTrack:
			vid = tr.ID
		case mkv.AudioTrack:
			auds = append(auds, tr.ID)
		}
	}
	if vid == 0 || len(auds) < 2 {
		t.Fatalf("fixture needs 1 video + 2 audio, got video=%d audio=%v", vid, auds)
	}

	// Baseline: both audio renditions present.
	full := filepath.Join(t.TempDir(), "full")
	if err := RemuxToHLS(context.Background(), src, full, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	master := readFile(t, filepath.Join(full, "master.m3u8"))
	if n := strings.Count(master, "TYPE=AUDIO"); n != 2 {
		t.Fatalf("baseline master has %d audio renditions, want 2", n)
	}

	// Virtual "one audio" version: keep video + the first audio only.
	one := filepath.Join(t.TempDir(), "one")
	if err := RemuxToHLS(context.Background(), src, one, Options{SegmentMs: 2000, KeepTracks: []uint64{vid, auds[0]}}); err != nil {
		t.Fatal(err)
	}
	m2 := readFile(t, filepath.Join(one, "master.m3u8"))
	if n := strings.Count(m2, "TYPE=AUDIO"); n != 1 {
		t.Errorf("KeepTracks master has %d audio renditions, want 1", n)
	}
	// The dropped audio's rendition playlist must not exist.
	if _, err := os.Stat(filepath.Join(one, "audio2.m3u8")); err == nil {
		t.Error("dropped audio rendition (audio2.m3u8) was still written")
	}
}

// TestKeepTracksPlanParity proves the on-demand plan applies KeepTracks
// identically to the full pass: a video-only virtual version (drop the audio via
// KeepTracks) is byte-identical, and keeping no video errors.
func TestKeepTracksPlanParity(t *testing.T) {
	src, _ := buildLacedFixture(t) // video (id 1) + laced audio (id 2), with Cues
	opts := Options{SegmentMs: 2000, KeepTracks: []uint64{1}}

	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, opts); err != nil {
		t.Fatal(err)
	}
	if m := readFile(t, filepath.Join(dir, "master.m3u8")); strings.Contains(m, "TYPE=AUDIO") {
		t.Error("KeepTracks={video} still advertises an audio rendition")
	}
	plan, err := PlanHLS(context.Background(), src, opts)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range plan.Resources() {
		if name == "master.m3u8" || name == "manifest.mpd" {
			continue // estimated BANDWIDTH on Matroska plans
		}
		got, _, err := plan.Resource(context.Background(), name)
		if err != nil {
			t.Errorf("Resource(%q): %v", name, err)
			continue
		}
		want, ferr := os.ReadFile(filepath.Join(dir, name))
		if ferr != nil {
			t.Errorf("full pass did not write %s", name)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the full pass (KeepTracks diverged)", name)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no resources compared")
	}
	if _, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000, KeepTracks: []uint64{2}}); err == nil {
		t.Error("PlanHLS keeping no video should error")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
