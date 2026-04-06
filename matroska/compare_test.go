package matroska

import (
	"context"
	"os"
	"testing"
)

func TestCompareIdentical(t *testing.T) {
	requireFixture(t)
	diffs, err := Compare(context.Background(), fixturePath, fixturePath)
	assertNoErr(t, err)
	assertEqual(t, len(diffs), 0, "diffs count")
}

func TestCompareTitle(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	edited := dir + "/edited.mkv"
	assertNoErr(t, EditMetadata(context.Background(), fixturePath, edited, func(c *Container) {
		c.Info.Title = "New Title"
	}))

	diffs, err := Compare(context.Background(), fixturePath, edited)
	assertNoErr(t, err)

	found := false
	for _, d := range diffs {
		if d.Section == "info.title" && d.Type == DiffChanged {
			found = true
		}
	}
	if !found {
		t.Error("expected title diff")
	}
}

func TestCompareTrackAdded(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()

	videoOnly := dir + "/video.mkv"
	assertNoErr(t, Mux(context.Background(), MuxOptions{
		OutputPath: videoOnly,
		Tracks:     []TrackInput{{SourcePath: fixturePath, TrackID: 1, IsDefault: true}},
	}))

	diffs, err := Compare(context.Background(), videoOnly, fixturePath)
	assertNoErr(t, err)

	hasAdded := false
	for _, d := range diffs {
		if d.Type == DiffAdded {
			hasAdded = true
			t.Logf("diff: %s", d)
		}
	}
	if !hasAdded {
		t.Error("expected added track diff")
	}
}

func TestCompareTrackRemoved(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()

	videoOnly := dir + "/video.mkv"
	assertNoErr(t, Mux(context.Background(), MuxOptions{
		OutputPath: videoOnly,
		Tracks:     []TrackInput{{SourcePath: fixturePath, TrackID: 1, IsDefault: true}},
	}))

	diffs, err := Compare(context.Background(), fixturePath, videoOnly)
	assertNoErr(t, err)

	hasRemoved := false
	for _, d := range diffs {
		if d.Type == DiffRemoved {
			hasRemoved = true
		}
	}
	if !hasRemoved {
		t.Error("expected removed track diff")
	}
}

func TestCompareTrackLanguage(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	edited := dir + "/edited.mkv"
	assertNoErr(t, EditMetadata(context.Background(), fixturePath, edited, func(c *Container) {
		c.Tracks[1].Language = "fre"
	}))

	diffs, err := Compare(context.Background(), fixturePath, edited)
	assertNoErr(t, err)

	found := false
	for _, d := range diffs {
		if d.Section == "track[2].language" {
			found = true
			t.Logf("diff: %s", d)
		}
	}
	if !found {
		t.Errorf("expected language diff, got %v", diffs)
	}
}

func TestCompareChapters(t *testing.T) {
	dir := t.TempDir()

	write := func(name string, chapters []Chapter) string {
		p := dir + "/" + name
		f, _ := os.Create(p)
		Write(f, &Container{
			Info:     SegmentInfo{TimecodeScale: 1000000, MuxingApp: "t", WritingApp: "t"},
			Tracks:   []Track{{ID: 1, Type: VideoTrack, Codec: "h264", Language: "und"}},
			Chapters: chapters,
		})
		f.Close()
		return p
	}

	a := write("a.mkv", []Chapter{{ID: 1, Title: "Intro", StartMs: 0}})
	b := write("b.mkv", []Chapter{{ID: 1, Title: "Opening", StartMs: 0}, {ID: 2, Title: "Main", StartMs: 5000}})

	diffs, err := Compare(context.Background(), a, b)
	assertNoErr(t, err)

	hasCountDiff := false
	hasTitleDiff := false
	for _, d := range diffs {
		if d.Section == "chapters.count" {
			hasCountDiff = true
		}
		if d.Section == "chapter[1].title" {
			hasTitleDiff = true
		}
	}
	if !hasCountDiff {
		t.Error("expected chapters count diff")
	}
	if !hasTitleDiff {
		t.Error("expected chapter title diff")
	}
}
