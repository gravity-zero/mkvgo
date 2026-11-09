package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// A second edit of a file whose Info an earlier edit moved to the Segment's end is the one every reader sees, and
// leaves one Info in the file: it appended a second copy behind the first, which the readers kept returning.
func TestEditInPlaceTwiceOnATightHeadKeepsTheLastEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tight.mkv")
	tightHeadMKV(t, path, nil)
	ctx := context.Background()
	first, second := "A"+strings.Repeat("a", 300), "B"+strings.Repeat("b", 600)
	mustNil(t, EditInPlace(ctx, path, func(c *mkv.Container) { c.Info.Title = first }))
	mustNil(t, EditInPlace(ctx, path, func(c *mkv.Container) { c.Info.Title = second }))
	full, err := reader.Open(ctx, path)
	mustNil(t, err)
	meta, err := reader.OpenMeta(ctx, path)
	mustNil(t, err)
	if full.Info.Title != second || meta.Info.Title != second {
		t.Errorf("titles after the second edit: full read %.1q..., head read %.1q..., want B", full.Info.Title, meta.Info.Title)
	}
	for _, name := range []string{"N" + strings.Repeat("n", 900), "M" + strings.Repeat("m", 1800)} {
		mustNil(t, EditInPlace(ctx, path, func(c *mkv.Container) { c.Tracks[0].Name = name }))
		full, err = reader.Open(ctx, path)
		mustNil(t, err)
		meta, err = reader.OpenMeta(ctx, path)
		mustNil(t, err)
		if full.Tracks[0].Name != name || meta.Tracks[0].Name != name || full.Info.Title != second || meta.Info.Title != second {
			t.Errorf("after naming the track %.1q...: full read %.1q... title %.1q..., head read %.1q... title %.1q...", name,
				full.Tracks[0].Name, full.Info.Title, meta.Tracks[0].Name, meta.Info.Title)
		}
	}
	data, err := os.ReadFile(path)
	mustNil(t, err)
	if n := bytes.Count(data, []byte(first)); n != 0 {
		t.Errorf("the first edit's title is still in the file %d time(s)", n)
	}
	if n := bytes.Count(data, []byte("N"+strings.Repeat("n", 900))); n != 0 {
		t.Errorf("the first track name is still in the file %d time(s)", n)
	}
}
