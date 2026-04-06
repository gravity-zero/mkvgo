package matroska

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestBlockReader(t *testing.T) {
	c := requireFixture(t)
	counts := countBlocks(t, fixturePath, c.Info.TimecodeScale)

	if len(counts) == 0 {
		t.Fatal("no blocks found")
	}
	for _, track := range c.Tracks {
		if counts[track.ID] == 0 {
			t.Errorf("track %d (%s) has no blocks", track.ID, track.Type)
		}
	}
	t.Logf("blocks per track: %v", counts)
}

func TestDemux(t *testing.T) {
	c := requireFixture(t)
	dir := t.TempDir()

	assertNoErr(t, Demux(context.Background(), DemuxOptions{
		SourcePath: fixturePath, OutputDir: dir,
	}))

	for _, track := range c.Tracks {
		path := filepath.Join(dir, fmt.Sprintf("%d.%s", track.ID, track.Codec))
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("track %d (%s): missing output: %v", track.ID, track.Codec, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("track %d (%s): empty output", track.ID, track.Codec)
		}
		t.Logf("track %d (%s %s): %d bytes", track.ID, track.Type, track.Codec, info.Size())
	}
}

func TestDemuxSelectTracks(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()

	assertNoErr(t, Demux(context.Background(), DemuxOptions{
		SourcePath: fixturePath, OutputDir: dir, TrackIDs: []uint64{1},
	}))

	entries, _ := os.ReadDir(dir)
	assertEqual(t, len(entries), 1, "output file count")
}

func TestMux(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	outPath := filepath.Join(dir, "muxed.mkv")

	assertNoErr(t, Mux(context.Background(), MuxOptions{
		OutputPath: outPath,
		Tracks: []TrackInput{
			{SourcePath: fixturePath, TrackID: 1, Language: "und", Name: "Video", IsDefault: true},
			{SourcePath: fixturePath, TrackID: 2, Language: "eng", Name: "Audio", IsDefault: true},
		},
	}))

	c, err := Open(context.Background(), outPath)
	assertNoErr(t, err)

	assertEqual(t, len(c.Tracks), 2, "tracks count")
	assertEqual(t, c.Tracks[0].Type, VideoTrack, "track[0].Type")
	assertEqual(t, c.Tracks[1].Type, AudioTrack, "track[1].Type")
	assertEqual(t, c.Tracks[0].Name, "Video", "track[0].Name")

	counts := countBlocks(t, outPath, c.Info.TimecodeScale)
	t.Logf("blocks per track: %v", counts)
	if counts[1] == 0 || counts[2] == 0 {
		t.Error("missing blocks")
	}
}

func TestMuxSingleTrack(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	outPath := filepath.Join(dir, "video_only.mkv")

	assertNoErr(t, Mux(context.Background(), MuxOptions{
		OutputPath: outPath,
		Tracks:     []TrackInput{{SourcePath: fixturePath, TrackID: 1, Language: "und", IsDefault: true}},
	}))

	c, err := Open(context.Background(), outPath)
	assertNoErr(t, err)
	assertEqual(t, len(c.Tracks), 1, "tracks count")
	assertEqual(t, c.Tracks[0].Type, VideoTrack, "track type")
}
