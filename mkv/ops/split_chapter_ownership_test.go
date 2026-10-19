package ops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildChapteredMKV writes a single-cluster MKV with chapters, video keyframes
// every kfEveryMs and frames every 500 ms.
func buildChapteredMKV(t *testing.T, dir, name string, chapters []mkv.Chapter, durationMs, kfEveryMs int64) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info:     mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test"},
		Chapters: chapters,
	}
	if err := mw.WriteMetadata(c, []mkv.Track{videoTrack(1)}, durationMs); err != nil {
		t.Fatal(err)
	}
	var blocks []mkv.Block
	for tc := int64(0); tc < durationMs; tc += 500 {
		blocks = append(blocks, mkv.Block{
			TrackNumber: 1, Timecode: tc, Keyframe: tc%kfEveryMs == 0, Data: []byte("v"),
		})
	}
	if err := mw.WriteClusterWithCues(0, 1_000_000, blocks); err != nil {
		t.Fatal(err)
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

func chapterTimes(t *testing.T, path string) []int64 {
	t.Helper()
	c, err := reader.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int64, len(c.Chapters))
	for i, ch := range c.Chapters {
		out[i] = ch.StartMs
	}
	return out
}

// A chapter marker belongs to the part that holds the FRAME it names. A cut
// lands on the first keyframe at or after the bound it was asked for, and the
// part before the cut keeps the GOP straddling it - so a marker sitting between
// the requested bound and that keyframe names a frame the PREVIOUS part holds.
// Selected on the requested bound it came to the next part anyway, with nowhere
// to sit but zero, and the film came back with every chapter announced up to a
// GOP early - at a picture that is not the one it names.
//
// Chapters at 0 / 3000 / 7000, keyframes every 2000: two of the three markers
// fall inside the overlap. Rejoined, the film has to name the same instants it
// was cut on.
func TestSplitJoin_ChapterKeepsThePictureItNames(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	src := buildChapteredMKV(t, dir, "src.mkv", []mkv.Chapter{
		{ID: 1, Title: "one", StartMs: 0},
		{ID: 2, Title: "two", StartMs: 3000},
		{ID: 3, Title: "three", StartMs: 7000},
	}, 10000, 2000)
	want := chapterTimes(t, src)

	parts, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src,
		OutputDir:  filepath.Join(dir, "parts"),
		ByChapters: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d", len(parts))
	}

	// Each part opens on the chapter that is playing when it starts: alone, a
	// part still says what it holds.
	for i, p := range parts {
		times := chapterTimes(t, p)
		if len(times) == 0 || times[0] != 0 {
			t.Errorf("part %d chapters = %v, want one opening at 0", i+1, times)
		}
	}

	joined := filepath.Join(dir, "joined.mkv")
	if err := Join(ctx, parts, joined); err != nil {
		t.Fatal(err)
	}
	assertTimecodes(t, "joined chapters", chapterTimes(t, joined), want)
}

// The part before the cut is the one that holds the frame, so it is the one
// that carries the marker; the part after the cut opens in the middle of that
// same chapter and says so at its own zero. Both statements are true and both
// are written - it is the JOIN that has to know the second is the first still
// running.
func TestSplit_TheOverlapCarriesItsChapter(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	src := buildChapteredMKV(t, dir, "src.mkv", []mkv.Chapter{
		{ID: 1, Title: "one", StartMs: 0},
		{ID: 2, Title: "two", StartMs: 3000},
	}, 8000, 2000)

	parts, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src,
		OutputDir:  filepath.Join(dir, "parts"),
		Ranges:     []mkv.TimeRange{{StartMs: 0, EndMs: 3000}, {StartMs: 3000, EndMs: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Part 1 runs to the keyframe at 4000, so it holds 3000..4000 - the first
	// second of chapter two - and names it at 3000.
	assertTimecodes(t, "part 1 chapters", chapterTimes(t, parts[0]), []int64{0, 3000})
	// Part 2 opens at 4000, in the middle of chapter two.
	assertTimecodes(t, "part 2 chapters", chapterTimes(t, parts[1]), []int64{0})
}

// A repeated chapter is a continuation only between parts of ONE timeline. Two
// files that merely follow each other keep both, UID collisions renumbered:
// dropping one there would be an editorial call on somebody else's chapters.
func TestJoin_RepeatedChapterOnlyMergesAcrossALink(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	tracks := []mkv.Track{videoTrack(1)}
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")},
		{TrackNumber: 1, Timecode: 500, Keyframe: false, Data: []byte("v")},
	}
	chapters := []mkv.Chapter{{ID: 7, Title: "same", StartMs: 0}}

	uidA := bytes.Repeat([]byte{0xA1}, 16)
	uidB := bytes.Repeat([]byte{0xB2}, 16)

	for _, tc := range []struct {
		name    string
		prevOfB []byte
		want    int
	}{
		{"linked", uidA, 1},
		{"unlinked", nil, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := filepath.Join(dir, tc.name)
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			a := buildLinkedMKV(t, sub, "a.mkv", tracks, blocks, 1000, uidA, nil, uidB, chapters...)
			b := buildLinkedMKV(t, sub, "b.mkv", tracks, blocks, 1000, uidB, tc.prevOfB, nil, chapters...)

			dst := filepath.Join(sub, "joined.mkv")
			if err := Join(ctx, []string{a, b}, dst); err != nil {
				t.Fatal(err)
			}
			if got := chapterTimes(t, dst); len(got) != tc.want {
				t.Errorf("joined chapters = %v (%d), want %d", got, len(got), tc.want)
			}
		})
	}
}

// The head books room before the blocks say which chapters the part ends up
// holding, so the booking has to be an upper bound on that list - the slot is
// sized for the widest timestamp EBML can encode, so only the LIST ever needs
// bounding. A part that gains a chapter from its overlap must not overflow it.
func TestSplit_ChapterSlotHoldsWhatTheOverlapAdds(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Twelve chapters with long titles, none of them landing on a keyframe: each
	// part reaches past its own end into the next marker and has to carry it, so
	// every slot is booked for one atom more than the range asked for.
	var chapters []mkv.Chapter
	for i := int64(0); i < 12; i++ {
		start := i * 2500
		if i > 0 {
			start -= 100 // just short of the keyframe at i*2500
		}
		chapters = append(chapters, mkv.Chapter{
			ID: uint64(i + 1), Title: fmt.Sprintf("chapter %02d - a title long enough to matter", i+1),
			StartMs: start,
		})
	}
	src := buildChapteredMKV(t, dir, "src.mkv", chapters, 30000, 500)

	parts, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src, OutputDir: filepath.Join(dir, "parts"), ByChapters: true,
	})
	if err != nil {
		t.Fatal(err) // "chapters need N bytes, slot holds M" is the failure this guards
	}
	joined := filepath.Join(dir, "joined.mkv")
	if err := Join(ctx, parts, joined); err != nil {
		t.Fatal(err)
	}
	assertTimecodes(t, "joined chapters", chapterTimes(t, joined), chapterTimes(t, src))
}

// A booking made before the blocks said where the cut fell can find every
// candidate out of range: a chapter that ends before the part's first keyframe
// belongs to the previous part, and this one writes NO chapters at all. The
// booked slot must come back as plain Void - filling it with a Chapters element
// whose EditionEntry holds no atom put a spec-invalid element at the end of the
// index, on every such part.
func TestSplit_EmptyChapterSelectionLeavesNoElement(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// One chapter at 1000-1400; keyframes at 0 and 5000 only. Part 2 is cut at
	// 1000 but really starts at 5000: the chapter is booked (it overlaps the
	// requested range) and then out of range (it ends before the cut).
	src := buildChapteredMKV(t, dir, "src.mkv", []mkv.Chapter{
		{ID: 1, Title: "one", StartMs: 1000, EndMs: 1400},
	}, 10000, 5000)

	parts, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src,
		OutputDir:  filepath.Join(dir, "parts"),
		Ranges:     []mkv.TimeRange{{StartMs: 0, EndMs: 1000}, {StartMs: 1000, EndMs: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The chapter's frame is in part 2's overlap? No: it ends at 1400, before
	// part 2's first frame at 5000 - it belongs to part 1 alone.
	if got := chapterTimes(t, parts[0]); len(got) != 1 {
		t.Errorf("part 1 chapters = %v, want the one chapter", got)
	}
	c, err := reader.Open(ctx, parts[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Chapters) != 0 {
		t.Errorf("part 2 chapters = %d, want none", len(c.Chapters))
	}
	// And no Chapters ELEMENT either: an empty EditionEntry is not "no
	// chapters", it is an invalid element other readers stumble on.
	raw, err := os.ReadFile(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte{0x10, 0x43, 0xA7, 0x70}) {
		t.Error("part 2 carries a Chapters element with nothing in it; the slot must be Void")
	}
}

// The sizing pass of a Join has no measured extents, so it must not dedup a
// continued chapter either: the extents decide WHICH copy survives the write -
// a phantom copy past a source's real end is clipped, letting the next part's
// copy through - and a slot sized on the other copy can be too small for it.
// Sizing keeps every copy; the write's list is then always a subset.
func TestJoin_ContinuedChapterFitsTheSlotWhicheverCopySurvives(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}

	var blocks []mkv.Block
	for tc := int64(0); tc <= 2000; tc += 500 {
		blocks = append(blocks,
			mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: tc == 0, Data: []byte("v")},
			mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")},
		)
	}

	uidA := bytes.Repeat([]byte{0xA1}, 16)
	uidB := bytes.Repeat([]byte{0xB2}, 16)
	// A declares a chapter PAST everything it holds (a phantom tail marker,
	// UID 7, short title): the write pass clips it at the seam.
	a := buildLinkedMKV(t, dir, "a.mkv", tracks, blocks, 2500, uidA, nil, uidB,
		mkv.Chapter{ID: 7, Title: "x", StartMs: 9000})
	// B, the continuation, carries the same UID with a much larger encoding:
	// this is the copy the write pass keeps, and the slot must hold it.
	b := buildLinkedMKV(t, dir, "b.mkv", tracks, blocks, 2500, uidB, uidA, nil,
		mkv.Chapter{
			ID: 7, Title: "the same chapter, stated at length so its atom outgrows the phantom's",
			StartMs: 0,
			SubChapters: []mkv.Chapter{
				{ID: 71, Title: "first movement", StartMs: 0},
				{ID: 72, Title: "second movement", StartMs: 1000},
			},
		})

	dst := filepath.Join(dir, "joined.mkv")
	if err := Join(ctx, []string{a, b}, dst); err != nil {
		t.Fatal(err) // "chapters need N bytes, slot holds M" is the failure this guards
	}
	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	// The phantom was clipped, B's real copy survived, stated once.
	if len(c.Chapters) != 1 || c.Chapters[0].Title == "x" {
		t.Errorf("joined chapters = %+v, want B's single full statement", c.Chapters)
	}
}

// The booking and the selection are bounded by the SAME value, so they cannot
// disagree. Where a cut does reach past that bound - chapters closer together
// than the source's keyframes, a split that cannot work - the part keeps the
// chapters it was asked for and the failure is reported by the range that has
// no keyframe. Letting the selection run past the booking instead had the first
// part fail on a slot that came up short, which says nothing about the cause.
func TestSplit_ChaptersCloserThanTheKeyframesFailOnTheRange(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	src := buildChapteredMKV(t, dir, "src.mkv", []mkv.Chapter{
		{ID: 1, Title: "one", StartMs: 0},
		{ID: 2, Title: "two", StartMs: 1000},
		{ID: 3, Title: "three", StartMs: 2000},
	}, 9000, 3000)

	_, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src, OutputDir: filepath.Join(dir, "parts"), ByChapters: true,
	})
	if err == nil {
		t.Fatal("a range with no keyframe in it must be an error")
	}
	if !strings.Contains(err.Error(), "no video keyframe") {
		t.Errorf("error is %q, want the range's own diagnosis (no video keyframe)", err)
	}
}

// TestSplitByChaptersKeepsTheHeadBeforeTheFirstChapter: frames before the first chapter's start belong to the first part, none is lost.
func TestSplitByChaptersKeepsTheHeadBeforeTheFirstChapter(t *testing.T) {
	dir := t.TempDir()
	src := buildChapteredMKV(t, dir, "src.mkv", []mkv.Chapter{{ID: 1, Title: "A", StartMs: 1000}, {ID: 2, Title: "B", StartMs: 4000}}, 6000, 2000)
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "out"), ByChapters: true})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, p := range parts {
		total += countBlocksFromFile(t, p, 1_000_000)[1]
	}
	if want := countBlocksFromFile(t, src, 1_000_000)[1]; total != want {
		t.Errorf("the parts hold %d frames of the source's %d", total, want)
	}
}

// TestSplitRefusesRangesPastTheEndOrInverted: a range past the end or ending before it starts is refused before any part is written.
func TestSplitRefusesRangesPastTheEndOrInverted(t *testing.T) {
	dir := t.TempDir()
	src := buildChapteredMKV(t, dir, "src.mkv", nil, 6000, 2000)
	for _, tc := range []struct {
		r    []mkv.TimeRange
		want string
	}{
		{[]mkv.TimeRange{{StartMs: 0, EndMs: 2000}, {StartMs: 9000, EndMs: 10000}}, "past the end"},
		{[]mkv.TimeRange{{StartMs: 0, EndMs: 2000}, {StartMs: 4000, EndMs: 3000}}, "before it starts"},
	} {
		out := filepath.Join(t.TempDir(), "out")
		_, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: out, Ranges: tc.r})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ranges %v: err = %v, want %q", tc.r, err, tc.want)
		}
		if entries, _ := os.ReadDir(out); len(entries) != 0 {
			t.Errorf("ranges %v: a refused split wrote %d parts", tc.r, len(entries))
		}
	}
}

// TestSplitByChaptersRunsTheLastPartThroughAChapterWithNoKeyframe: a chapter after the last keyframe stays in the part before, as in MP4.
func TestSplitByChaptersRunsTheLastPartThroughAChapterWithNoKeyframe(t *testing.T) {
	dir := t.TempDir()
	src := buildChapteredMKV(t, dir, "src.mkv", []mkv.Chapter{{ID: 1, Title: "A", StartMs: 0}, {ID: 2, Title: "B", StartMs: 2000}, {ID: 3, Title: "Credits", StartMs: 5000}}, 6000, 2000)
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "out"), ByChapters: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("%d parts, want 2", len(parts))
	}
	total := 0
	for _, p := range parts {
		total += countBlocksFromFile(t, p, 1_000_000)[1]
	}
	if want := countBlocksFromFile(t, src, 1_000_000)[1]; total != want {
		t.Errorf("the parts hold %d frames of the source's %d", total, want)
	}
}

// TestSplitKeepsEverySoundBlockStoredPastTheCut: however many sound blocks are stored after the cut keyframe yet timed before it, the part before gets them all.
func TestSplitKeepsEverySoundBlockStoredPastTheCut(t *testing.T) {
	dir := t.TempDir()
	blocks := []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("k0")}}
	for tc := int64(0); tc < 600; tc++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")})
	}
	blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("k1")})
	for tc := int64(600); tc < 2000; tc++ { // 400 of them timed before the keyframe stored ahead of them
		blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")})
	}
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1), audioTrack(2)}, blocks, 2000)
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "out"), Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 1000}, {StartMs: 1000}}})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, p := range parts {
		total += countBlocksFromFile(t, p, 1_000_000)[2]
	}
	if total != 2000 {
		t.Errorf("the parts hold %d sound blocks of the source's 2000", total)
	}
}

// TestSplitCutsOnTheFirstVideoTrack: a second, all-intra video track does not open a part in the middle of the first one's GOP.
func TestSplitCutsOnTheFirstVideoTrack(t *testing.T) {
	dir := t.TempDir()
	var blocks []mkv.Block
	for tc := int64(0); tc < 4000; tc += 500 {
		blocks = append(blocks,
			mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: tc%2000 == 0, Data: []byte("v")},
			mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("i")})
	}
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1), videoTrack(2)}, blocks, 4000)
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "out"), Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 1000}, {StartMs: 1000}}})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	for {
		blk, err := br.Next()
		if err != nil {
			t.Fatalf("part 2 holds no block of the first video track: %v", err)
		}
		if blk.TrackNumber == 1 {
			if !blk.Keyframe {
				t.Error("part 2 opens the first video track on a picture that is not a keyframe")
			}
			return
		}
	}
}

// TestSplitByChaptersKeepsGapsAndTheTail: chapters whose stated ends leave a gap, or stop before the end, lose no frame.
func TestSplitByChaptersKeepsGapsAndTheTail(t *testing.T) {
	dir := t.TempDir()
	src := buildChapteredMKV(t, dir, "src.mkv", []mkv.Chapter{{ID: 1, Title: "A", StartMs: 0, EndMs: 1000}, {ID: 2, Title: "B", StartMs: 2000, EndMs: 3000}}, 6000, 2000)
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "out"), ByChapters: true})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, p := range parts {
		total += countBlocksFromFile(t, p, 1_000_000)[1]
	}
	if want := countBlocksFromFile(t, src, 1_000_000)[1]; total != want {
		t.Errorf("the parts hold %d frames of the source's %d", total, want)
	}
}

// TestSplitRefusesSoundStoredPastWhatACutHolds: past the bound behindCut holds, the cut is refused, not the sound dropped.
func TestSplitRefusesSoundStoredPastWhatACutHolds(t *testing.T) {
	defer func(n int) { behindCutMaxBytes = n }(behindCutMaxBytes)
	behindCutMaxBytes = 100
	dir := t.TempDir()
	blocks := []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("k0")}, {TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte("k1")}}
	for tc := int64(0); tc < 1000; tc++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")})
	}
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1), audioTrack(2)}, blocks, 2000)
	_, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "out"), Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 1000}, {StartMs: 1000}}})
	if err == nil || !strings.Contains(err.Error(), "interleave") {
		t.Errorf("err = %v, want a refusal of sound stored too far past the cut", err)
	}
}

// TestSplitOpensASecondVideoTrackOnItsOwnKeyframe: a second video track with its own GOP (an angle) opens each part on
// its own keyframe, as MP4 does, never on pictures that need the GOP before the cut.
func TestSplitOpensASecondVideoTrackOnItsOwnKeyframe(t *testing.T) {
	dir := t.TempDir()
	var blocks []mkv.Block
	for tc := int64(0); tc < 4000; tc += 250 {
		blocks = append(blocks,
			mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: tc%1000 == 0, Data: []byte("v")},
			mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: tc%1500 == 0, Data: []byte("w")})
	}
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1), videoTrack(2)}, blocks, 4000)
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "out"), Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 2000}, {StartMs: 2000}}})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	for {
		blk, err := br.Next()
		if err != nil {
			t.Fatalf("part 2 holds no block of the second video track: %v", err)
		}
		if blk.TrackNumber == 2 {
			if !blk.Keyframe {
				t.Errorf("part 2 opens the second video track at %d ms on a picture that is not a keyframe", blk.Timecode)
			}
			break
		}
	}
	// The pictures between the cut and that keyframe stay in part 1: a join gives every block back where it was.
	joined := filepath.Join(dir, "joined.mkv")
	if err := Join(context.Background(), parts, joined); err != nil {
		t.Fatal(err)
	}
	if want, got := blocksByTrack(t, src), blocksByTrack(t, joined); fmt.Sprint(want) != fmt.Sprint(got) {
		t.Errorf("split then join:\n got %v\nwant %v", got, want)
	}
	if c, err := reader.Open(context.Background(), parts[0]); err != nil || c.DurationMs != 2000 {
		t.Errorf("part 1 declares %v ms (%v), want the cut track's 2000", c.DurationMs, err)
	}
}

// blocksByTrack lists each track's blocks of a file as "timecode:data", in file order.
func blocksByTrack(t *testing.T, path string) map[uint64][]string {
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
	out := map[uint64][]string{}
	for {
		blk, err := br.Next()
		if err != nil {
			return out
		}
		out[blk.TrackNumber] = append(out[blk.TrackNumber], fmt.Sprintf("%d:%s", blk.Timecode, blk.Data))
	}
}
