package ops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// A tail Tags element declaring more bytes than the file holds (a file cut short, a forged size) is not voided over
// its declared size: the edit wrote that many bytes, past the end of the file, from one allocation of the same size.
func TestEditInPlaceVoidsNoTailElementPastTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cut.mkv")
	tailTagsMKV(t, path, 2)
	whole, err := os.Stat(path)
	mustNil(t, err)
	mustNil(t, os.Truncate(path, whole.Size()-100))
	err = EditInPlace(context.Background(), path, func(c *mkv.Container) {
		c.Tags = []mkv.Tag{{TargetType: "MOVIE", SimpleTags: []mkv.SimpleTag{{Name: "A", Value: "b"}}}}
	})
	after, serr := os.Stat(path)
	mustNil(t, serr)
	if after.Size() > whole.Size()-100 {
		t.Errorf("the edit (err %v) grew a file of %d bytes to %d", err, whole.Size()-100, after.Size())
	}
}
