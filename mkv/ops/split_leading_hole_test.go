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

// A keyframe-aligned part begins on the first keyframe at or after the bound it
// was cut on, which the source puts wherever its GOP falls. Rebasing its blocks
// on the REQUESTED bound instead left the part opening on a hole exactly as long
// as that distance: played alone it froze on its first frame, and joined back
// every seam gained the same gap - the whole file drifting a little further out
// at each one.
//
// Three things have to agree on where the part really starts: its blocks, the
// duration it declares, and its chapters. This checks all three, then joins the
// parts back and requires the source's timeline, frame for frame.
func TestSplit_PartStartsOnItsFirstFrame(t *testing.T) {
	dir := t.TempDir()

	// Video keyframes every 2000 ms, frames every 500 ms; audio every 500 ms.
	// A chapter opens on the keyframe at 6000, which is where the second part
	// really starts although it is cut at 5000.
	var blocks []mkv.Block
	for tc := int64(0); tc < 10000; tc += 500 {
		blocks = append(blocks,
			mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: tc%2000 == 0, Data: []byte("v")},
			mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")},
		)
	}
	src := buildMinimalMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1), audioTrack(2)}, blocks, 10000)
	srcVideo := collectTimecodes(t, src, 1_000_000, 1)

	ctx := context.Background()
	parts, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src,
		OutputDir:  filepath.Join(dir, "parts"),
		Ranges:     []mkv.TimeRange{{StartMs: 0, EndMs: 5000}, {StartMs: 5000, EndMs: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}

	// The second part holds the source from 6000 on: its own timeline starts
	// there, on both tracks at once.
	assertTimecodes(t, "part 2 video", collectTimecodes(t, parts[1], 1_000_000, 1),
		[]int64{0, 500, 1000, 1500, 2000, 2500, 3000, 3500})
	assertTimecodes(t, "part 2 audio", collectTimecodes(t, parts[1], 1_000_000, 2),
		[]int64{0, 500, 1000, 1500, 2000, 2500, 3000, 3500})

	// Each part declares what it holds - the first keeps the GOP straddling its
	// end, so it runs past the 5000 it was asked for - and together they account
	// for the whole source rather than for the ranges.
	for i, want := range []int64{6000, 4000} {
		c, err := reader.Open(ctx, parts[i])
		if err != nil {
			t.Fatal(err)
		}
		if c.DurationMs != want {
			t.Errorf("part %d declares %d ms, want %d", i+1, c.DurationMs, want)
		}
	}

	// Joined back, nothing has moved: the parts are contiguous, so the seam
	// reproduces the source's timeline exactly instead of drifting by the
	// distance to the keyframe.
	joined := filepath.Join(dir, "joined.mkv")
	if err := Join(ctx, parts, joined); err != nil {
		t.Fatal(err)
	}
	assertTimecodes(t, "joined video", collectTimecodes(t, joined, 1_000_000, 1), srcVideo)

	c, err := reader.Open(ctx, joined)
	if err != nil {
		t.Fatal(err)
	}
	if c.DurationMs != 10000 {
		t.Errorf("joined file declares %d ms, want the source's 10000", c.DurationMs)
	}
}

// The chapters of a part must be measured from the same instant its picture is:
// clipChapters rebases on the requested bound, so a part whose first frame lands
// later has to close that difference, or the marker sits ahead of the picture it
// names by however far the GOP reached.
func TestSplit_ChaptersFollowThePartsFirstFrame(t *testing.T) {
	dir := t.TempDir()

	// The chapter opens on the keyframe at 6000, the frame the second part
	// really begins on although it is cut at 5000.
	path := filepath.Join(dir, "src.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test"},
		Chapters: []mkv.Chapter{
			{ID: 1, Title: "one", StartMs: 0, EndMs: 5000},
			{ID: 2, Title: "two", StartMs: 6000, EndMs: 10000},
		},
	}
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteMetadata(c, []mkv.Track{videoTrack(1)}, 10000); err != nil {
		t.Fatal(err)
	}
	for tc := int64(0); tc < 10000; tc += 500 {
		blk := mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: tc%2000 == 0, Data: []byte("v")}
		if err := mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{blk}); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	parts, err := Split(ctx, mkv.SplitOptions{
		SourcePath: path,
		OutputDir:  filepath.Join(dir, "parts"),
		Ranges:     []mkv.TimeRange{{StartMs: 0, EndMs: 5000}, {StartMs: 5000, EndMs: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}

	part, err := reader.Open(ctx, parts[1])
	if err != nil {
		t.Fatal(err)
	}
	c = part
	if len(c.Chapters) != 1 {
		t.Fatalf("part 2 has %d chapters, want 1", len(c.Chapters))
	}
	// The part's first frame IS the chapter's frame: both are at zero.
	if c.Chapters[0].StartMs != 0 {
		t.Errorf("chapter %q at %d ms, want 0 - the part opens on the frame it names",
			c.Chapters[0].Title, c.Chapters[0].StartMs)
	}
	first := collectTimecodes(t, parts[1], 1_000_000, 1)
	if len(first) == 0 || first[0] != c.Chapters[0].StartMs {
		t.Errorf("first frame at %v, chapter at %d - they must agree", first[:1], c.Chapters[0].StartMs)
	}
}

// A source that reorders across the gate - the audio of the keyframe's instant
// arriving after it in file order, slightly behind it in time - must still come
// out with non-negative timestamps: the part's zero is the smallest timecode of
// its first cluster, not the first block that happened to open the gate.
func TestSplit_LateBlocksBehindTheCutKeyframe(t *testing.T) {
	dir := t.TempDir()

	var blocks []mkv.Block
	for tc := int64(0); tc < 10000; tc += 500 {
		blocks = append(blocks, mkv.Block{
			TrackNumber: 1, Timecode: tc, Keyframe: tc%2000 == 0, Data: []byte("v"),
		})
		// Audio for the same instant, written 100 ms behind the picture and
		// AFTER it in file order.
		blocks = append(blocks, mkv.Block{
			TrackNumber: 2, Timecode: tc - 100, Keyframe: true, Data: []byte("a"),
		})
	}
	blocks[1].Timecode = 0 // no negative timecode in the source itself
	src := buildMinimalMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1), audioTrack(2)}, blocks, 10000)

	parts, err := Split(context.Background(), mkv.SplitOptions{
		SourcePath: src,
		OutputDir:  filepath.Join(dir, "parts"),
		Ranges:     []mkv.TimeRange{{StartMs: 5000, EndMs: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The audio at 5900 sits behind the keyframe at 6000 that opened the part,
	// so the part's zero is 5900 and nothing lands before it.
	for _, track := range []uint64{1, 2} {
		for _, tc := range collectTimecodes(t, parts[0], 1_000_000, track) {
			if tc < 0 {
				t.Fatalf("track %d has a block at %d ms", track, tc)
			}
		}
	}
	video := collectTimecodes(t, parts[0], 1_000_000, 1)
	audio := collectTimecodes(t, parts[0], 1_000_000, 2)
	if len(video) == 0 || len(audio) == 0 {
		t.Fatalf("empty part: video %v audio %v", video, audio)
	}
	if audio[0] != 0 {
		t.Errorf("audio starts at %d, want 0 - it is the earliest block kept", audio[0])
	}
	if video[0] != 100 {
		t.Errorf("video starts at %d, want 100 - 100 ms after the audio, as in the source", video[0])
	}
}

func TestSplit_PartFileIsReadable(t *testing.T) {
	dir := t.TempDir()
	var blocks []mkv.Block
	for tc := int64(0); tc < 4000; tc += 500 {
		blocks = append(blocks, mkv.Block{
			TrackNumber: 1, Timecode: tc, Keyframe: tc%2000 == 0, Data: []byte("v"),
		})
	}
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, blocks, 4000)
	parts, err := Split(context.Background(), mkv.SplitOptions{
		SourcePath: src, OutputDir: filepath.Join(dir, "parts"),
		Ranges: []mkv.TimeRange{{StartMs: 1000, EndMs: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(parts[0]); err != nil {
		t.Fatal(err)
	}
}
