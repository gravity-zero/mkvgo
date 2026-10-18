package mp4

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// chaptered is an edit fixture carrying chs.
func chaptered(t *testing.T, chs []mkv.Chapter) string {
	t.Helper()
	src := editFixture(t)
	if err := EditInPlace(context.Background(), src, func(c *mkv.Container) { c.Chapters = chs }); err != nil {
		t.Fatal(err)
	}
	return src
}

// chapterTrackCount is how many samples the file's QuickTime chapter track holds.
func chapterTrackCount(t *testing.T, path string) int {
	t.Helper()
	boxes := moovBoxesOf(t, path)
	ids := chapterTrackRefs(boxes)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range boxes {
		if tkhd, err := boxAt(b.payload, "tkhd"); b.typ == "trak" && err == nil && ids[tkhdTrackID(tkhd)] {
			return len(chapterTrackSamples(b.payload, fi.Size()))
		}
	}
	return 0
}

// splitLinkTags lists the split link atoms a file's moov still carries.
func splitLinkTags(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, name := range []string{"SPLIT_UID", "SPLIT_PREV"} {
		if strings.Contains(string(data), name) {
			found = append(found, name)
		}
	}
	return found
}

// TestJoinKeepsSameTitledChaptersOfUnrelatedFiles: files a split did not cut keep each its own chapter, whatever their titles.
func TestJoinKeepsSameTitledChaptersOfUnrelatedFiles(t *testing.T) {
	var files []string
	for range 3 {
		files = append(files, chaptered(t, []mkv.Chapter{{ID: 1, Title: "Chapter 1", StartMs: 0}}))
	}
	dst := filepath.Join(t.TempDir(), "j.mp4")
	if err := Join(context.Background(), files, dst); err != nil {
		t.Fatal(err)
	}
	if got := mustMeta(t, dst).Chapters; len(got) != 3 {
		t.Errorf("%d chapters, want one per file: %+v", len(got), got)
	}
}

// TestSplitJoinRoundTripLeavesNoLinkTags: a split's parts carry the link that lets a join give their chapters back, the join output none.
func TestSplitJoinRoundTripLeavesNoLinkTags(t *testing.T) {
	src := chaptered(t, []mkv.Chapter{{ID: 1, Title: "A", StartMs: 0}, {ID: 2, Title: "B", StartMs: 1500}})
	parts := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 3000}, {StartMs: 3000}}})
	for _, p := range parts {
		if len(splitLinkTags(t, p)) == 0 {
			t.Errorf("%s carries no split link", filepath.Base(p))
		}
	}
	dst := filepath.Join(t.TempDir(), "j.mp4")
	if err := Join(context.Background(), parts, dst); err != nil {
		t.Fatal(err)
	}
	if got := mustMeta(t, dst).Chapters; len(got) != 2 || got[1].Title != "B" || got[1].StartMs != 1500 {
		t.Errorf("chapters after split and join: %+v", got)
	}
	if tags := splitLinkTags(t, dst); len(tags) != 0 {
		t.Errorf("the join output still carries %v", tags)
	}
}

// TestJoinCapsChaptersAt255: the chpl and the chapter track of a join agree, at most 255 chapters.
func TestJoinCapsChaptersAt255(t *testing.T) {
	var chs []mkv.Chapter
	for i := range 200 {
		chs = append(chs, mkv.Chapter{ID: uint64(i + 1), Title: "c", StartMs: int64(i) * 25})
	}
	a, b := chaptered(t, chs), chaptered(t, chs)
	dst := filepath.Join(t.TempDir(), "j.mp4")
	if err := Join(context.Background(), []string{a, b}, dst); err != nil {
		t.Fatal(err)
	}
	n, track := len(mustMeta(t, dst).Chapters), chapterTrackCount(t, dst)
	if n != maxChapters || track != maxChapters {
		t.Errorf("chpl %d chapters, chapter track %d samples, want %d each", n, track, maxChapters)
	}
}
