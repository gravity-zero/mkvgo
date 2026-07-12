package ops

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestJoin_CodecMismatch covers the codec validation: joining tracks that line up
// by count and type but differ in codec (or codec-private) must be rejected, not
// silently produce a broken file.
func TestJoin_CodecMismatch(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	src1 := buildMinimalMKV(t, dir, "a.mkv", []mkv.Track{videoTrack(1)}, testBlocks(1), 200)

	hevc := videoTrack(1)
	hevc.Codec = "hevc" // same id/type, different codec
	src2 := buildMinimalMKV(t, dir, "b.mkv", []mkv.Track{hevc}, testBlocks(1), 200)
	if err := Join(ctx, []string{src1, src2}, filepath.Join(dir, "o1.mkv")); err == nil || !strings.Contains(err.Error(), "codec") {
		t.Fatalf("codec mismatch should be rejected, got %v", err)
	}

	other := videoTrack(1)
	other.CodecPrivate = []byte{0x02} // same codec, different configuration
	src3 := buildMinimalMKV(t, dir, "c.mkv", []mkv.Track{other}, testBlocks(1), 200)
	if err := Join(ctx, []string{src1, src3}, filepath.Join(dir, "o2.mkv")); err == nil || !strings.Contains(err.Error(), "codec configuration") {
		t.Fatalf("codec-private mismatch should be rejected, got %v", err)
	}
}

// TestJoin_PerTrackOffsets covers the per-track concatenation: when tracks end at
// different times, each must be rebased on its OWN end, so a joined track stays
// contiguous (≤ one-frame gap at the boundary) instead of drifting by the whole
// container duration.
func TestJoin_PerTrackOffsets(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Video (track 1) ends at 50; audio (track 2) ends at 120 - different ends.
	blocks := func() []mkv.Block {
		bs := []mkv.Block{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")},
			{TrackNumber: 1, Timecode: 50, Keyframe: true, Data: []byte("v")},
		}
		for tc := int64(0); tc <= 120; tc += 20 {
			bs = append(bs, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")})
		}
		return bs
	}
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	src1 := buildMinimalMKV(t, dir, "a.mkv", tracks, blocks(), 150)
	src2 := buildMinimalMKV(t, dir, "b.mkv", tracks, blocks(), 150)

	dst := filepath.Join(dir, "joined.mkv")
	if err := Join(ctx, []string{src1, src2}, dst); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	var video []int64
	for {
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if blk.TrackNumber == 1 {
			video = append(video, blk.Timecode)
		}
	}
	if len(video) != 4 {
		t.Fatalf("video blocks = %d, want 4 (%v)", len(video), video)
	}
	// Each consecutive gap must be ~one frame (≤ 60 ms); a per-file (container
	// duration ≈ 150 ms) offset would leave a ~100 ms gap at the join boundary.
	for i := 1; i < len(video); i++ {
		if gap := video[i] - video[i-1]; gap > 60 {
			t.Errorf("video gap at index %d = %d ms (want ≤ 60): %v - per-track rebase failed", i, gap, video)
		}
	}
}
