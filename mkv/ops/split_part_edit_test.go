package ops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// A part now carries a reserved Chapters slot in its head. Check the rest of the
// toolbox still accepts it: EditInPlace (which folds the head region), reindex
// in place, and a plain re-read.
func TestSplit_PartSurvivesEditInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "src.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test"},
		Chapters: []mkv.Chapter{
			{ID: 1, Title: "one", StartMs: 0, EndMs: 5000},
			{ID: 2, Title: "two", StartMs: 6000, EndMs: 10000},
		},
	}
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteMetadata(c, []mkv.Track{videoTrack(1), audioTrack(2)}, 10000); err != nil {
		t.Fatal(err)
	}
	for tc := int64(0); tc < 10000; tc += 500 {
		blocks := []mkv.Block{
			{TrackNumber: 1, Timecode: tc, Keyframe: tc%2000 == 0, Data: []byte("vvvv")},
			{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("aaaa")},
		}
		if err := mw.WriteClusterWithCues(tc, 1_000_000, blocks); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	ctx := context.Background()
	parts, err := Split(ctx, mkv.SplitOptions{
		SourcePath: path, OutputDir: filepath.Join(dir, "parts"),
		Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 5000}, {StartMs: 5000, EndMs: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}

	part := parts[1]
	if err := EditInPlace(ctx, part, func(c *mkv.Container) {
		c.Info.Title = "edited"
	}); err != nil {
		t.Fatalf("EditInPlace on a part: %v", err)
	}
	got, err := reader.Open(ctx, part)
	if err != nil {
		t.Fatalf("re-read after EditInPlace: %v", err)
	}
	if got.Info.Title != "edited" {
		t.Errorf("title = %q, want edited", got.Info.Title)
	}
	if len(got.Chapters) != 1 || got.Chapters[0].StartMs != 0 {
		t.Errorf("chapters after EditInPlace: %+v", got.Chapters)
	}
	if got.DurationMs != 4000 {
		t.Errorf("duration after EditInPlace = %d, want 4000", got.DurationMs)
	}
	first := collectTimecodes(t, part, 1_000_000, 1)
	if len(first) == 0 || first[0] != 0 {
		t.Errorf("first video block at %v, want 0", first)
	}
}
