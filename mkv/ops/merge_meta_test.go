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

// Merge's metadata policy is first-wins: title, chapters, tags AND attachments
// of the first input must survive into the output (attachments used to be
// dropped entirely — MuxOptions had no field for them).
func TestMerge_CarriesFirstInputMetadata(t *testing.T) {
	dir := t.TempDir()

	first := filepath.Join(dir, "first.mkv")
	f, err := os.Create(first)
	if err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{
			TimecodeScale: 1_000_000, Title: "Feature", MuxingApp: "test", WritingApp: "test",
		},
		Chapters: []mkv.Chapter{{ID: 1, Title: "Intro", StartMs: 0, EndMs: 500}},
		Attachments: []mkv.Attachment{
			{ID: 1, Name: "font.ttf", MIMEType: "font/ttf", Data: []byte("fontdata")},
		},
		Tags: []mkv.Tag{{TargetType: "MOVIE", SimpleTags: []mkv.SimpleTag{{Name: "ARTIST", Value: "someone"}}}},
	}
	tracks := []mkv.Track{videoTrack(1)}
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteMetadata(c, tracks, 1000); err != nil {
		t.Fatal(err)
	}
	// >50 blocks so the BlockReader's progress interval fires at least once.
	var blks []mkv.Block
	for tc := int64(0); tc < 600; tc += 10 {
		blks = append(blks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: []byte("v")})
	}
	if err := mw.WriteClusterWithCues(0, 1_000_000, blks); err != nil {
		t.Fatal(err)
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	second := buildMinimalMKV(t, dir, "second.mkv",
		[]mkv.Track{audioTrack(1)},
		[]mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("a")}},
		1000)

	out := filepath.Join(dir, "merged.mkv")
	var progressCalls int
	err = Merge(context.Background(), mkv.MergeOptions{
		OutputPath: out,
		Inputs:     []mkv.MergeInput{{SourcePath: first}, {SourcePath: second}},
		Progress:   func(processed, total int64) { progressCalls++ },
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := reader.Open(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Info.Title != "Feature" {
		t.Errorf("Title = %q, want %q (first input's title)", got.Info.Title, "Feature")
	}
	if len(got.Chapters) != 1 || got.Chapters[0].Title != "Intro" {
		t.Errorf("Chapters = %+v, want the first input's chapter", got.Chapters)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].Name != "font.ttf" ||
		string(got.Attachments[0].Data) != "fontdata" {
		t.Errorf("Attachments = %+v, want the first input's font.ttf", got.Attachments)
	}
	if len(got.Tags) == 0 {
		t.Errorf("Tags missing, want the first input's tags")
	}
	if len(got.Tracks) != 2 {
		t.Errorf("Tracks = %d, want 2 (one per input)", len(got.Tracks))
	}
	if progressCalls == 0 {
		t.Errorf("MergeOptions.Progress was never called")
	}
}
