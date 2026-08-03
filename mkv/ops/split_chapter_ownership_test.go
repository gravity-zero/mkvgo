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
