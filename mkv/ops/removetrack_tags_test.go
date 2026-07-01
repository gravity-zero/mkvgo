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

// RemoveTrack must drop the tags that target a removed track's UID — leaving
// them in would produce orphan tags pointing at a track that no longer exists.
// Global tags (TargetID 0) and tags on kept tracks survive.
func TestRemoveTrack_DropsTagsOfRemovedTrack(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test"},
		Tags: []mkv.Tag{
			{TargetType: "MOVIE", SimpleTags: []mkv.SimpleTag{{Name: "TITLE", Value: "global"}}},
			{TargetType: "TRACK", TargetID: 1, SimpleTags: []mkv.SimpleTag{{Name: "BPS", Value: "100"}}},
			{TargetType: "TRACK", TargetID: 2, SimpleTags: []mkv.SimpleTag{{Name: "BPS", Value: "200"}}},
		},
	}
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)} // UID defaults to ID at write time
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")},
		{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("a")},
	}
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteMetadata(c, tracks, 1000); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteClusterWithCues(0, 1_000_000, blocks); err != nil {
		t.Fatal(err)
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "out.mkv")
	if err := RemoveTrack(context.Background(), src, dst, []uint64{2}); err != nil {
		t.Fatal(err)
	}
	got, err := reader.Open(context.Background(), dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tags) != 2 {
		t.Fatalf("tags = %+v, want 2 (global + track 1; track 2's tag dropped)", got.Tags)
	}
	for _, tag := range got.Tags {
		if tag.TargetID == 2 {
			t.Errorf("orphan tag targeting removed track UID 2 survived: %+v", tag)
		}
	}
}
