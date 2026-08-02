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

func cont(chapters ...mkv.Chapter) *mkv.Container {
	return &mkv.Container{Chapters: chapters}
}

// writeChapteredMKV is buildMultiClusterMKV plus a chapter list, which Split
// needs to cut on.
func writeChapteredMKV(t *testing.T, path string, blockSets [][]mkv.Block, chapters []mkv.Chapter, durationMs int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{
		Info:     mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test"},
		Chapters: chapters,
	}, []mkv.Track{videoTrack(1)}, durationMs))
	for _, blocks := range blockSets {
		if len(blocks) == 0 {
			continue
		}
		mustNil(t, mw.WriteClusterWithCues(blocks[0].Timecode, 1_000_000, blocks))
	}
	mustNil(t, mw.Finalize())
}

// TestConcatChapters covers the four rules the merge commits to at once.
func TestConcatChapters(t *testing.T) {
	sources := []*mkv.Container{
		cont(
			mkv.Chapter{ID: 1, Title: "Intro", StartMs: 0, EndMs: 2000},
			mkv.Chapter{ID: 2, Title: "Chase", StartMs: 2000, EndMs: 5000},
		),
		cont(
			// Same UIDs as the first source: both were cut from one original.
			mkv.Chapter{ID: 1, Title: "Trial", StartMs: 0, EndMs: 3000},
			mkv.Chapter{ID: 7, Title: "Linked", StartMs: 3000, SegmentUID: []byte{0xAA}},
			mkv.Chapter{ID: 2, Title: "Finale", StartMs: 3000},
		),
	}
	// The second source really starts at 5120, not at the 5000 its range asked
	// for: the seam carries the straddling GOP.
	got := concatChapters(sources, []int64{0, 5120})

	want := []mkv.Chapter{
		{ID: 1, Title: "Intro", StartMs: 0, EndMs: 2000},
		{ID: 2, Title: "Chase", StartMs: 2000, EndMs: 5000},
		{ID: 3, Title: "Trial", StartMs: 5120, EndMs: 8120}, // UID 1 taken -> 3
		{ID: 4, Title: "Finale", StartMs: 8120},             // UID 2 taken -> 4, no end stays no end
	}
	if len(got) != len(want) {
		t.Fatalf("got %d chapters, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.ID != w.ID || g.Title != w.Title || g.StartMs != w.StartMs || g.EndMs != w.EndMs {
			t.Errorf("chapter %d = %+v, want %+v", i, g, w)
		}
	}

	// Both sides of a seam are kept even when they name the same moment.
	seam := concatChapters([]*mkv.Container{
		cont(mkv.Chapter{ID: 1, Title: "Part", StartMs: 0, EndMs: 1000}),
		cont(mkv.Chapter{ID: 1, Title: "Part", StartMs: 0, EndMs: 1000}),
	}, []int64{0, 1000})
	if len(seam) != 2 {
		t.Errorf("seam: got %d chapters, want both kept: %+v", len(seam), seam)
	}

	// Sizing pass: no offsets yet, same shape so the slot fits the real write.
	sizing := concatChapters(sources, nil)
	if len(sizing) != len(want) {
		t.Errorf("sizing pass yields %d chapters, want the same %d as the real pass", len(sizing), len(want))
	}
	if sizing[2].StartMs != 0 {
		t.Errorf("sizing pass shifted by %d, want 0", sizing[2].StartMs)
	}
}

// TestConcatChapters_SubChaptersRideAlong: nesting is shifted at every depth,
// and shares the one UID namespace.
func TestConcatChapters_SubChaptersRideAlong(t *testing.T) {
	got := concatChapters([]*mkv.Container{
		cont(mkv.Chapter{ID: 1, StartMs: 0, EndMs: 1000}),
		cont(mkv.Chapter{ID: 1, Title: "Act", StartMs: 100, EndMs: 900, SubChapters: []mkv.Chapter{
			{ID: 5, Title: "Scene", StartMs: 200, EndMs: 400},
		}}),
	}, []int64{0, 1000})

	if len(got) != 2 || len(got[1].SubChapters) != 1 {
		t.Fatalf("unexpected shape: %+v", got)
	}
	parent, sub := got[1], got[1].SubChapters[0]
	if parent.StartMs != 1100 || parent.EndMs != 1900 {
		t.Errorf("parent = [%d, %d], want [1100, 1900]", parent.StartMs, parent.EndMs)
	}
	if sub.StartMs != 1200 || sub.EndMs != 1400 {
		t.Errorf("sub-chapter = [%d, %d], want [1200, 1400]", sub.StartMs, sub.EndMs)
	}
	if parent.ID == got[0].ID {
		t.Errorf("parent kept the colliding UID %d", parent.ID)
	}
	if sub.ID != 5 {
		t.Errorf("sub-chapter UID = %d, want 5 kept (no collision)", sub.ID)
	}
}

// TestChapters_SubChaptersSurviveAWrite: nesting is parsed by the reader and
// carried by every op, but the writer used to emit only the top level, so a
// sub-chapter vanished at the first rewrite - split, join or a metadata edit.
func TestChapters_SubChaptersSurviveAWrite(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	src := filepath.Join(dir, "nested.mkv")
	writeChapteredMKV(t, src, [][]mkv.Block{{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")},
	}}, []mkv.Chapter{
		{ID: 1, Title: "Act I", StartMs: 0, EndMs: 4000, SubChapters: []mkv.Chapter{
			{ID: 2, Title: "Scene 1", StartMs: 0, EndMs: 2000},
			{ID: 3, Title: "Scene 2", StartMs: 2000, EndMs: 4000, SubChapters: []mkv.Chapter{
				{ID: 4, Title: "Beat", StartMs: 3000, EndMs: 4000},
			}},
		}},
	}, 4000)

	c, err := reader.OpenMeta(ctx, src, reader.WithChapters())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Chapters) != 1 {
		t.Fatalf("top level = %d chapters, want 1: %+v", len(c.Chapters), c.Chapters)
	}
	act := c.Chapters[0]
	if len(act.SubChapters) != 2 {
		t.Fatalf("%q has %d sub-chapters, want 2 - the writer dropped the nesting", act.Title, len(act.SubChapters))
	}
	if act.SubChapters[0].Title != "Scene 1" || act.SubChapters[1].Title != "Scene 2" {
		t.Errorf("sub-chapters = %q, %q", act.SubChapters[0].Title, act.SubChapters[1].Title)
	}
	if act.SubChapters[1].StartMs != 2000 || act.SubChapters[1].EndMs != 4000 {
		t.Errorf("Scene 2 = [%d, %d], want [2000, 4000]", act.SubChapters[1].StartMs, act.SubChapters[1].EndMs)
	}
	// Two levels down.
	if len(act.SubChapters[1].SubChapters) != 1 || act.SubChapters[1].SubChapters[0].Title != "Beat" {
		t.Errorf("second nesting level lost: %+v", act.SubChapters[1].SubChapters)
	}
}

// TestJoin_RestoresChaptersOfEverySource is the round trip the real corpus
// showed missing: split a file on its chapters, join the parts back, and get
// every chapter again - at the offsets the blocks were really written to, not
// the boundaries the split was asked for.
func TestJoin_RestoresChaptersOfEverySource(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// 12 s of video, keyframes every second, chapters at 0/4/8 s.
	var sets [][]mkv.Block
	for tc := int64(0); tc < 12000; tc += 500 {
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: tc, Keyframe: tc%1000 == 0, Data: []byte("v")},
		})
	}
	// No ChapterTimeEnd on any of them: that is what mainstream muxers write,
	// and each chapter simply runs until the next one starts.
	src := filepath.Join(dir, "src.mkv")
	writeChapteredMKV(t, src, sets, []mkv.Chapter{
		{ID: 11, Title: "One", StartMs: 0},
		{ID: 22, Title: "Two", StartMs: 4000},
		{ID: 33, Title: "Three", StartMs: 8000},
	}, 12000)

	parts, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src, OutputDir: filepath.Join(dir, "parts"), ByChapters: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 {
		t.Fatalf("parts = %v, want 3", parts)
	}
	for i, p := range parts {
		c, err := reader.OpenMeta(ctx, p, reader.WithChapters())
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Chapters) != 1 {
			t.Fatalf("part %d carries %d chapters, want its own 1", i+1, len(c.Chapters))
		}
	}

	dst := filepath.Join(dir, "rejoined.mkv")
	if err := Join(ctx, parts, dst); err != nil {
		t.Fatal(err)
	}
	c, err := reader.OpenMeta(ctx, dst, reader.WithChapters())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Chapters) != 3 {
		t.Fatalf("rejoined file has %d chapters, want 3 (one per part): %+v", len(c.Chapters), c.Chapters)
	}
	titles := []string{"One", "Two", "Three"}
	seen := map[uint64]bool{}
	var prev int64 = -1
	for i, ch := range c.Chapters {
		if ch.Title != titles[i] {
			t.Errorf("chapter %d title = %q, want %q", i, ch.Title, titles[i])
		}
		if seen[ch.ID] {
			t.Errorf("chapter %d reuses UID %d", i, ch.ID)
		}
		seen[ch.ID] = true
		if ch.StartMs <= prev {
			t.Errorf("chapter %d starts at %d, not after the previous %d - offsets were not applied",
				i, ch.StartMs, prev)
		}
		prev = ch.StartMs
	}
	// Each chapter marks where its part actually begins, which a keyframe-aligned
	// cut puts at or past the nominal boundary - never before it.
	for i, want := range []int64{0, 4000, 8000} {
		if c.Chapters[i].StartMs < want {
			t.Errorf("chapter %d starts at %d, before its part's nominal %d",
				i, c.Chapters[i].StartMs, want)
		}
	}
}
