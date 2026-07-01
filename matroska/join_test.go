package matroska

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildSplittableMKV writes a 2-track source with video keyframes at 0 and
// 500ms, so a [0,500)+[500,∞) split has a valid cut point (keyframe
// alignment).
func buildSplittableMKV(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	assertNoErr(t, err)
	c := &Container{
		Info: SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test"},
	}
	w, h := uint32(320), uint32(240)
	sr := 48000.0
	ch := uint8(2)
	tracks := []Track{
		{ID: 1, Type: VideoTrack, Codec: "h264", Width: &w, Height: &h, CodecPrivate: []byte{1}},
		{ID: 2, Type: AudioTrack, Codec: "aac", SampleRate: &sr, Channels: &ch},
	}
	// Interleaved by timecode, like a real muxer's cluster stream.
	var blocks []mkv.Block
	for tc := int64(0); tc < 1000; tc += 125 {
		if tc%250 == 0 {
			blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: tc%500 == 0, Data: []byte("v")})
		}
		blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")})
	}
	mw := writer.NewMKVWriter(f)
	assertNoErr(t, mw.WriteStart())
	assertNoErr(t, mw.WriteMetadata(c, tracks, 1000))
	for _, b := range blocks {
		assertNoErr(t, mw.WriteClusterWithCues(b.Timecode, 1_000_000, []mkv.Block{b}))
	}
	assertNoErr(t, mw.Finalize())
	assertNoErr(t, f.Close())
}

func TestJoin(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.mkv")
	buildSplittableMKV(t, srcPath)

	// Split in 2 at the 500ms keyframe, then join back
	parts, err := Split(context.Background(), SplitOptions{
		SourcePath: srcPath,
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

	// A cut at an exact keyframe boundary loses no block.
	origCounts := countBlocks(t, srcPath, c.Info.TimecodeScale)
	for id, origN := range origCounts {
		if counts[id] != origN {
			t.Errorf("track %d: joined=%d, original=%d — blocks lost at the seam", id, counts[id], origN)
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
