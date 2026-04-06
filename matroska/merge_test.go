package matroska

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMergeSingleSource(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "merged.mkv")

	assertNoErr(t, Merge(context.Background(), MergeOptions{
		OutputPath: out,
		Inputs:     []MergeInput{{SourcePath: fixturePath}},
	}))

	c, err := Open(context.Background(), out)
	assertNoErr(t, err)
	assertEqual(t, len(c.Tracks), 2, "tracks")

	counts := countBlocks(t, out, c.Info.TimecodeScale)
	if counts[1] == 0 || counts[2] == 0 {
		t.Errorf("missing blocks: %v", counts)
	}
}

func TestMergeFilteredTracks(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "filtered.mkv")

	assertNoErr(t, Merge(context.Background(), MergeOptions{
		OutputPath: out,
		Inputs:     []MergeInput{{SourcePath: fixturePath, TrackIDs: []uint64{1}}},
	}))

	c, err := Open(context.Background(), out)
	assertNoErr(t, err)
	assertEqual(t, len(c.Tracks), 1, "tracks")
	assertEqual(t, c.Tracks[0].Type, VideoTrack, "track type")
}

func TestMergeMultiSource(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()

	// Create two single-track MKVs
	videoMKV := filepath.Join(dir, "video.mkv")
	audioMKV := filepath.Join(dir, "audio.mkv")

	assertNoErr(t, Mux(context.Background(), MuxOptions{
		OutputPath: videoMKV,
		Tracks:     []TrackInput{{SourcePath: fixturePath, TrackID: 1, IsDefault: true}},
	}))
	assertNoErr(t, Mux(context.Background(), MuxOptions{
		OutputPath: audioMKV,
		Tracks:     []TrackInput{{SourcePath: fixturePath, TrackID: 2, IsDefault: true}},
	}))

	out := filepath.Join(dir, "combined.mkv")
	assertNoErr(t, Merge(context.Background(), MergeOptions{
		OutputPath: out,
		Inputs: []MergeInput{
			{SourcePath: videoMKV},
			{SourcePath: audioMKV},
		},
	}))

	c, err := Open(context.Background(), out)
	assertNoErr(t, err)
	assertEqual(t, len(c.Tracks), 2, "tracks")
	assertEqual(t, c.Tracks[0].Type, VideoTrack, "track[0] type")
	assertEqual(t, c.Tracks[1].Type, AudioTrack, "track[1] type")
}

func TestMergeWithSubtitles(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()

	srtPath := filepath.Join(dir, "test.srt")
	assertNoErr(t, os.WriteFile(srtPath, []byte(`1
00:00:00,100 --> 00:00:01,000
Hello
`), 0644))

	out := filepath.Join(dir, "with_sub.mkv")
	assertNoErr(t, MergeWithSubtitles(context.Background(), fixturePath, srtPath, out, "eng", "Test", nil))

	c, err := Open(context.Background(), out)
	assertNoErr(t, err)
	assertEqual(t, len(c.Tracks), 3, "tracks")
	assertEqual(t, c.Tracks[2].Type, SubtitleTrack, "sub track")
	assertEqual(t, c.Tracks[2].Language, "eng", "sub lang")
}

func TestMergeNoInputs(t *testing.T) {
	err := Merge(context.Background(), MergeOptions{OutputPath: "/dev/null"})
	if err == nil {
		t.Fatal("expected error")
	}
}
