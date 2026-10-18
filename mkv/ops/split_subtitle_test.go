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

// subtitleBlocks reads a file's subtitle cues as (time, duration) pairs.
func subtitleBlocks(t *testing.T, path string, track uint64) [][2]int64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	var out [][2]int64
	for {
		b, err := br.Next()
		if err != nil {
			return out
		}
		if b.TrackNumber == track {
			out = append(out, [2]int64{b.Timecode, b.Duration})
		}
	}
}

// TestSplitCarriesASubtitleAcrossTheCut pins what a cue on screen across a split's cut becomes: shown from the
// start of the next part for the time it has left, and one cue again once the parts are joined.
func TestSplitCarriesASubtitleAcrossTheCut(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	sub := mkv.Track{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"}
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}, []mkv.Track{videoTrack(1), sub}, 4000))
	for tc := int64(0); tc < 4000; tc += 500 {
		blocks := []mkv.Block{{TrackNumber: 1, Timecode: tc, Duration: 500, Keyframe: tc%1000 == 0, Data: []byte("v")}}
		if tc == 1500 {
			blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: 1500, Duration: 1000, Keyframe: true, Data: []byte("across the cut")})
		}
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, blocks))
	}
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())
	ctx := context.Background()
	parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 2000}, {StartMs: 2000, EndMs: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	first, second := subtitleBlocks(t, parts[0], 2), subtitleBlocks(t, parts[1], 2)
	t.Logf("part 1 cues %v, part 2 cues %v", first, second)
	if len(second) != 1 || second[0] != [2]int64{0, 500} {
		t.Errorf("part 2 cues %v: want the cue on screen across the cut, from 0 for its last 500 ms", second)
	}
	joined := filepath.Join(dir, "joined.mkv")
	mustNil(t, Join(ctx, parts, joined))
	if got := subtitleBlocks(t, joined, 2); len(got) != 1 || got[0] != [2]int64{1500, 1000} {
		t.Errorf("joined cues %v: want the one cue back, 1500 ms for 1000 ms", got)
	}
}

// TestSplitCarriesASubtitleOverTwoCuts pins a cue on screen across two cuts: it opens both parts after the first,
// and the parts joined give it back once.
func TestSplitCarriesASubtitleOverTwoCuts(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	sub := mkv.Track{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"}
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}, []mkv.Track{videoTrack(1), sub}, 5000))
	for tc := int64(0); tc < 5000; tc += 500 {
		blocks := []mkv.Block{{TrackNumber: 1, Timecode: tc, Duration: 500, Keyframe: tc%1000 == 0, Data: []byte("v")}}
		if tc == 1500 {
			blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: 1500, Duration: 2000, Keyframe: true, Data: []byte("over two cuts")})
		}
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, blocks))
	}
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())
	ctx := context.Background()
	parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"),
		Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 2000}, {StartMs: 2000, EndMs: 3000}, {StartMs: 3000, EndMs: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	if second, third := subtitleBlocks(t, parts[1], 2), subtitleBlocks(t, parts[2], 2); len(second) != 1 || second[0] != [2]int64{0, 1500} || len(third) != 1 || third[0] != [2]int64{0, 500} {
		t.Errorf("parts 2 and 3 cues %v, %v: want the cue opening both, for 1500 then 500 ms", second, third)
	}
	for i, p := range parts {
		t.Logf("part %d cues %v", i+1, subtitleBlocks(t, p, 2))
	}
	joined := filepath.Join(dir, "joined.mkv")
	mustNil(t, Join(ctx, parts, joined))
	if got := subtitleBlocks(t, joined, 2); len(got) != 1 || got[0] != [2]int64{1500, 2000} {
		t.Errorf("joined cues %v: want the one cue back, 1500 ms for 2000 ms", got)
	}
	for i, v := range subtitleBlocks(t, joined, 1) { // the picture runs on at each seam, the cue's time holds no frame
		if v[0] != int64(i)*500 {
			t.Fatalf("joined frame %d at %d ms, want %d: a seam opened a hole", i, v[0], i*500)
		}
	}
}
