package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// TestSplitByChaptersNamesTheChapterWithoutAKeyframe pins the advice a chapter split gives when a chapter holds
// no keyframe: the chapter by name, and the ways out the caller has, never a range they did not give.
func TestSplitByChaptersNamesTheChapterWithoutAKeyframe(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"},
		Chapters: []mkv.Chapter{{StartMs: 0, Title: "Intro"}, {StartMs: 2500, Title: "Short"}, {StartMs: 2700, Title: "Rest"}}}
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(c, []mkv.Track{videoTrack(1)}, 4000))
	for tc := int64(0); tc < 4000; tc += 500 {
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{{TrackNumber: 1, Timecode: tc, Duration: 500, Keyframe: tc%1000 == 0, Data: []byte("v")}}))
	}
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())
	_, err = Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), ByChapters: true})
	if err == nil || !strings.Contains(err.Error(), `"Short"`) || !strings.Contains(err.Error(), "split by time instead") {
		t.Errorf("err = %v, want the chapter named and a split by time offered", err)
	}
}

// TestDropEndedCuesKeepsOnlyWhatCanRunThroughACut: a walk holds the cues still on screen, not every cue of the part.
func TestDropEndedCuesKeepsOnlyWhatCanRunThroughACut(t *testing.T) {
	cues := []mkv.Block{{Timecode: 0, Duration: 500}, {Timecode: 400, Duration: 700}, {Timecode: 900, Duration: 100}, {Timecode: 1000, Duration: 50}}
	kept := dropEndedCues(cues, 1000)
	if len(kept) != 2 || kept[0].Timecode != 400 || kept[1].Timecode != 1000 {
		t.Errorf("kept %+v, want the cues at 400 and 1000", kept)
	}
}

// TestSplitWithoutVideoCarriesTheCueOverTheCut: with no video track the cues ended long before a cut are let go, not the one it runs through.
func TestSplitWithoutVideoCarriesTheCueOverTheCut(t *testing.T) {
	dir := t.TempDir()
	var blocks []mkv.Block
	for tc := int64(0); tc < 6000; tc += 20 {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: []byte("a")})
		switch tc {
		case 500, 1500:
			blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Duration: 300, Data: []byte("Gone")})
		case 2500:
			blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Duration: 1000, Data: []byte("Across")})
		}
	}
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{audioTrack(1), subtitleTrack(2, "srt")}, blocks, 6000)
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "out"), Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 3000}, {StartMs: 3000}}})
	if err != nil {
		t.Fatal(err)
	}
	subs := trackBlocks(collectAllBlocks(t, parts[1]))[2]
	if len(subs) == 0 || subs[0].Timecode != 0 || string(subs[0].Data) != "Across" {
		t.Errorf("part 2 opens its subtitles on %+v, want the cue across the cut at 0", subs)
	}
}
