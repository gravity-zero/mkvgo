package ops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// negativeAudioMKV writes a video track 1 and an audio track 2 whose first two blocks are timed before 0 (-40 and
// -20 ms, a codec delay), at negative offsets from a cluster at 0; it returns the audio timecodes.
func negativeAudioMKV(t *testing.T, path string) []int64 {
	f, err := os.Create(path)
	mustNil(t, err)
	defer f.Close()
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}, []mkv.Track{videoTrack(1), audioTrack(2)}, 2000))
	var audio []int64
	for cl := int64(0); cl < 2000; cl += 1000 {
		var blocks []mkv.Block
		if cl == 0 {
			for _, tc := range []int64{-40, -20} {
				blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte{0xAA, byte(tc)}})
				audio = append(audio, tc)
			}
		}
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: cl, Keyframe: true, Data: make([]byte, 100)})
		for tc := cl; tc < cl+1000; tc += 20 {
			blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte{0xAA, byte(tc)}})
			audio = append(audio, tc)
		}
		mustNil(t, mw.WriteClusterWithCues(cl, 1_000_000, blocks))
	}
	mustNil(t, mw.Finalize())
	return audio
}

func trackTimecodes(t *testing.T, path string, track uint64) []int64 {
	var out []int64
	for _, b := range collectAllBlocks(t, path) {
		if b.TrackNumber == track {
			out = append(out, b.Timecode)
		}
	}
	return out
}

// TestRewriteKeepsBlocksTimedBeforeZero: the rewriting ops took a cluster timestamp of -1 for "none open yet", so a
// block timed before 0 reopened the cluster at each block, and the cluster was written at a negative timestamp, which
// the unsigned field wrapped to 2^64 - 40. The blocks keep their times, at negative offsets from a cluster at 0.
func TestRewriteKeepsBlocksTimedBeforeZero(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	want := negativeAudioMKV(t, src)
	same := func(name string, got []int64) {
		if len(got) != len(want) {
			t.Errorf("%s: %d audio blocks, want %d (%v...)", name, len(got), len(want), got[:min(4, len(got))])
			return
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: audio block %d at %d ms, want %d", name, i, got[i], want[i])
				return
			}
		}
	}

	removed := filepath.Join(dir, "removed.mkv")
	mustNil(t, RemoveTrack(context.Background(), src, removed, []uint64{1}))
	same("RemoveTrack", trackTimecodes(t, removed, 1)) // the audio, renumbered 1

	videoOnly := filepath.Join(dir, "video.mkv")
	mustNil(t, RemoveTrack(context.Background(), src, videoOnly, []uint64{2}))
	added := filepath.Join(dir, "added.mkv")
	mustNil(t, AddTrack(context.Background(), videoOnly, added, mkv.TrackInput{SourcePath: src, TrackID: 2}))
	var audio uint64
	for _, b := range collectAllBlocks(t, added) {
		if b.TrackNumber != 1 {
			audio = b.TrackNumber
			break
		}
	}
	same("AddTrack", trackTimecodes(t, added, audio))
}
