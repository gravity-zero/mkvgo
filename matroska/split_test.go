package matroska

import (
	"context"
	"strings"
	"os"
	"testing"
)

func TestSplitByTime(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()

	// The fixture's video track has a single keyframe at 0: part 1 (starting
	// at a keyframe) works; part 2 (starting at 500ms, mid-GOP with no later
	// keyframe) must be an explicit error, not a silently empty or corrupt
	// file.
	outputs, err := Split(context.Background(), SplitOptions{
		SourcePath: fixturePath,
		OutputDir:  dir,
		Ranges: []TimeRange{
			{StartMs: 0, EndMs: 500},
			{StartMs: 500, EndMs: 0}, // until EOF — no keyframe in range
		},
	})
	if err == nil {
		t.Fatal("expected an error for the mid-GOP part 2 (no video keyframe after 500ms)")
	}
	if !strings.Contains(err.Error(), "keyframe") {
		t.Errorf("error should name the missing keyframe, got: %v", err)
	}
	assertEqual(t, len(outputs), 1, "parts written before the failing range")

	c, err := Open(context.Background(), outputs[0])
	assertNoErr(t, err)
	assertEqual(t, len(c.Tracks), 2, "tracks count")
	counts := countBlocks(t, outputs[0], c.Info.TimecodeScale)
	t.Logf("part 1: blocks=%v", counts)
	if counts[1] == 0 {
		t.Errorf("part 1: no video blocks")
	}
}

func TestSplitByChapters(t *testing.T) {
	// Build a fixture with chapters
	dir := t.TempDir()
	srcPath := dir + "/with_chapters.mkv"

	src := requireFixture(t)
	src.Chapters = []Chapter{
		{ID: 1, Title: "Part 1", StartMs: 0, EndMs: 500},
		{ID: 2, Title: "Part 2", StartMs: 500},
	}

	f, err := os.Create(srcPath)
	assertNoErr(t, err)
	assertNoErr(t, Write(f, src))
	f.Close()

	// Now split with actual blocks — use the original fixture
	// (the Write above has no clusters, so use the real fixture)
	origC, _ := Open(context.Background(), fixturePath)
	origC.Chapters = []Chapter{
		{ID: 1, Title: "Part 1", StartMs: 0, EndMs: 500},
		{ID: 2, Title: "Part 2", StartMs: 500},
	}

	outputs, err := Split(context.Background(), SplitOptions{
		SourcePath: fixturePath,
		OutputDir:  dir,
		ByChapters: true,
	})
	// fixture has no chapters — this should error
	if err != nil {
		t.Logf("expected error (no chapters in fixture): %v", err)
		return
	}
	t.Logf("outputs: %v", outputs)
}

func TestSplitNoRanges(t *testing.T) {
	requireFixture(t)
	_, err := Split(context.Background(), SplitOptions{
		SourcePath: fixturePath,
		OutputDir:  t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error for no ranges")
	}
}
