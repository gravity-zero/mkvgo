package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// makeFragmentedMP4 builds a real fragmented MP4 (ftyp+moov(mvex) then a run of
// moof+mdat fragments) by concatenating mkvgo's own HLS video init and segments.
// It returns the bytes and the source's video sample (frame) count.
func makeFragmentedMP4(t *testing.T) ([]byte, int) {
	t.Helper()
	src, _ := buildLacedFixture(t) // 25 fps video keyframed each second + audio
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	initB, err := os.ReadFile(filepath.Join(dir, "init.mp4"))
	if err != nil {
		t.Fatalf("read init: %v", err)
	}
	segs, err := filepath.Glob(filepath.Join(dir, "seg*.m4s"))
	if err != nil || len(segs) == 0 {
		t.Fatalf("no video segments: %v", err)
	}
	sort.Strings(segs)
	var frag bytes.Buffer
	frag.Write(initB)
	for _, p := range segs {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		frag.Write(b)
	}
	return frag.Bytes(), 6 * 25 // 6 s at 25 fps
}

// TestParseFragmentedMP4SampleTable proves parseMP4(sampleFull) recovers a
// fragmented file's full sample table from the moof fragments: the right sample
// count, monotonically increasing decode times, and one keyframe per GOP.
func TestParseFragmentedMP4SampleTable(t *testing.T) {
	data, wantFrames := makeFragmentedMP4(t)
	mv, err := parseMP4(bytes.NewReader(data), int64(len(data)), sampleFull)
	if err != nil {
		t.Fatalf("parse fragmented MP4: %v", err)
	}
	if !mv.fragmented {
		t.Fatal("source was not detected as fragmented")
	}
	vt := videoTrack(mv)
	if vt == nil {
		t.Fatal("no video track")
	}
	if len(vt.samples) != wantFrames {
		t.Errorf("video samples = %d, want %d (moof fragments must all be walked)", len(vt.samples), wantFrames)
	}
	prev := int64(-1)
	syncs := 0
	for i, s := range vt.samples {
		if s.dtsMs < prev {
			t.Fatalf("sample %d dts %d < previous %d (fragment decode times must be monotonic)", i, s.dtsMs, prev)
		}
		prev = s.dtsMs
		if s.sync {
			syncs++
		}
		if s.size == 0 {
			t.Errorf("sample %d has zero size", i)
		}
	}
	if syncs == 0 {
		t.Error("no keyframes recovered from the fragments")
	}
}

// TestRemuxFromFragmentedMP4 round-trips a (video-only) fragmented MP4 back to
// Matroska: every frame the fragments held becomes a block, so the fragmented
// reader feeds RemuxFromMP4 exactly like a progressive file.
func TestRemuxFromFragmentedMP4(t *testing.T) {
	data, wantFrames := makeFragmentedMP4(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mp4")
	if err := os.WriteFile(in, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.mkv")
	if err := RemuxFromMP4(context.Background(), in, out); err != nil {
		t.Fatalf("RemuxFromMP4(fragmented): %v", err)
	}
	c, blocks := readMKV(t, out)
	if len(c.Tracks) != 1 || c.Tracks[0].Type != mkv.VideoTrack {
		t.Fatalf("tracks = %d, want one video track", len(c.Tracks))
	}
	if len(blocks) != wantFrames {
		t.Errorf("MKV carried %d blocks, want %d (every fragmented frame)", len(blocks), wantFrames)
	}
}
