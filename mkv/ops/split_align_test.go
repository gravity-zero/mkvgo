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

// Keyframe alignment must key on VIDEO keyframes: every audio block is flagged
// keyframe, so gating on "any keyframe" used to open the segment on the first
// audio block and then admit mid-GOP video (corrupt until the next real
// keyframe). The segment must start at the first video keyframe at/after
// StartMs, dropping earlier audio too, and end at the next video keyframe
// at/after EndMs so the straddling GOP loses no frame.
func TestSplit_KeyframeAlignGatesOnVideoTrack(t *testing.T) {
	dir := t.TempDir()

	var blocks []mkv.Block
	// Video (track 1): keyframes at 0, 2000, 4000; deltas every 500 ms.
	for tc := int64(0); tc <= 4500; tc += 500 {
		blocks = append(blocks, mkv.Block{
			TrackNumber: 1, Timecode: tc, Keyframe: tc%2000 == 0, Data: []byte("v"),
		})
	}
	// Audio (track 2): a block every 500 ms, all keyframes (as real audio is).
	for tc := int64(250); tc <= 4250; tc += 500 {
		blocks = append(blocks, mkv.Block{
			TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a"),
		})
	}
	sortBlocksByTimecode(blocks)
	src := buildMinimalMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1), audioTrack(2)}, blocks, 5000)

	files, err := Split(context.Background(), mkv.SplitOptions{
		SourcePath: src,
		OutputDir:  filepath.Join(dir, "parts"),
		Ranges:     []mkv.TimeRange{{StartMs: 1000, EndMs: 3000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 output, got %d", len(files))
	}

	video := collectTimecodes(t, files[0], 1_000_000, 1)
	audio := collectTimecodes(t, files[0], 1_000_000, 2)

	// Start: nothing before the first video keyframe >= 1000 (at 2000) - in
	// particular no leading audio, and no mid-GOP video from 1000/1500.
	// End: the GOP straddling 3000 is kept up to (excluding) the next video
	// keyframe at 4000.
	//
	// Output timecodes are rebased on the first frame KEPT (2000), not on the
	// requested bound (1000). Rebasing on the bound - which this test used to
	// require, freezing the defect - opened the part with a hole as long as the
	// distance to the keyframe: 1 s of nothing here, 2.4 to 5.5 s on real films
	// with long GOPs. Played alone the part froze on its first frame; joined
	// back, every seam gained that same gap. The A/V relationship is what must
	// survive, and it does: audio still sits 250 ms behind the picture.
	wantVideo := []int64{0, 500, 1000, 1500}
	wantAudio := []int64{250, 750, 1250, 1750}
	assertTimecodes(t, "video", video, wantVideo)
	assertTimecodes(t, "audio", audio, wantAudio)
}

// Audio-only sources keep the old behaviour: any keyframe starts the segment,
// the cut happens at EndMs.
func TestSplit_KeyframeAlignAudioOnly(t *testing.T) {
	dir := t.TempDir()
	var blocks []mkv.Block
	for tc := int64(0); tc <= 3000; tc += 500 {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: []byte("a")})
	}
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{audioTrack(1)}, blocks, 3500)

	files, err := Split(context.Background(), mkv.SplitOptions{
		SourcePath: src,
		OutputDir:  filepath.Join(dir, "parts"),
		Ranges:     []mkv.TimeRange{{StartMs: 1000, EndMs: 2000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := collectTimecodes(t, files[0], 1_000_000, 1)
	assertTimecodes(t, "audio", got, []int64{0, 500}) // rebased to segment timeline
}

// Each split segment must carry only the chapters overlapping its range,
// rebased to the segment's own timeline - not the source's full list at
// absolute timestamps.
func TestSplit_ChaptersClippedAndRebased(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "src.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test"},
		Chapters: []mkv.Chapter{
			{ID: 1, Title: "Before", StartMs: 0, EndMs: 1000},
			{ID: 2, Title: "Straddle", StartMs: 500, EndMs: 1500},
			{ID: 3, Title: "Inside", StartMs: 1500, EndMs: 2500},
			{ID: 4, Title: "TailClipped", StartMs: 2900, EndMs: 4000},
			{ID: 5, Title: "After", StartMs: 3000, EndMs: 5000},
		},
	}
	tracks := []mkv.Track{videoTrack(1)}
	var blocks []mkv.Block
	for tc := int64(0); tc <= 5000; tc += 500 {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: []byte("v")})
	}
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteMetadata(c, tracks, 5500); err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks {
		if err := mw.WriteClusterWithCues(b.Timecode, 1_000_000, []mkv.Block{b}); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	files, err := Split(context.Background(), mkv.SplitOptions{
		SourcePath: path,
		OutputDir:  filepath.Join(dir, "parts"),
		Ranges:     []mkv.TimeRange{{StartMs: 1000, EndMs: 3000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Open(context.Background(), files[0])
	if err != nil {
		t.Fatal(err)
	}

	want := []mkv.Chapter{
		{Title: "Straddle", StartMs: 0, EndMs: 500},
		{Title: "Inside", StartMs: 500, EndMs: 1500},
		{Title: "TailClipped", StartMs: 1900, EndMs: 2000},
	}
	if len(got.Chapters) != len(want) {
		t.Fatalf("chapters = %+v, want %d entries (Before/After excluded)", got.Chapters, len(want))
	}
	for i, w := range want {
		g := got.Chapters[i]
		if g.Title != w.Title || g.StartMs != w.StartMs || g.EndMs != w.EndMs {
			t.Errorf("chapter[%d] = {%q %d-%d}, want {%q %d-%d}",
				i, g.Title, g.StartMs, g.EndMs, w.Title, w.StartMs, w.EndMs)
		}
	}
}

func sortBlocksByTimecode(blocks []mkv.Block) {
	for i := 1; i < len(blocks); i++ {
		for j := i; j > 0 && blocks[j].Timecode < blocks[j-1].Timecode; j-- {
			blocks[j], blocks[j-1] = blocks[j-1], blocks[j]
		}
	}
}

func assertTimecodes(t *testing.T, label string, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s timecodes = %v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s timecodes = %v, want %v", label, got, want)
		}
	}
}
