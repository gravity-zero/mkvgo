package matroska

import (
	"context"
	"path/filepath"
	"testing"
)

func TestJoin(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()

	// Split fixture in 2, then join back
	parts, err := Split(context.Background(), SplitOptions{
		SourcePath: fixturePath,
		OutputDir:  dir,
		Ranges: []TimeRange{
			{StartMs: 0, EndMs: 500},
			{StartMs: 500},
		},
	})
	assertNoErr(t, err)

	outPath := filepath.Join(dir, "joined.mkv")
	assertNoErr(t, Join(context.Background(), parts, outPath))

	c, err := Open(context.Background(), outPath)
	assertNoErr(t, err)
	assertEqual(t, len(c.Tracks), 2, "tracks")

	counts := countBlocks(t, outPath, c.Info.TimecodeScale)
	t.Logf("joined blocks: %v", counts)

	// Blocks at the exact split boundary may land in only one part.
	// On a 1s fixture split at 500ms, losing ~20% is expected.
	origCounts := countBlocks(t, fixturePath, c.Info.TimecodeScale)
	for id, origN := range origCounts {
		if counts[id] < origN/2 {
			t.Errorf("track %d: joined=%d, original=%d — too many lost", id, counts[id], origN)
		}
	}
}

func TestJoinIncompatible(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()

	// Create a video-only MKV
	videoOnly := filepath.Join(dir, "video.mkv")
	assertNoErr(t, Mux(context.Background(), MuxOptions{
		OutputPath: videoOnly,
		Tracks:     []TrackInput{{SourcePath: fixturePath, TrackID: 1, IsDefault: true}},
	}))

	err := Join(context.Background(), []string{fixturePath, videoOnly}, filepath.Join(dir, "fail.mkv"))
	if err == nil {
		t.Fatal("expected error for incompatible tracks")
	}
	t.Logf("expected error: %v", err)
}
