package mp4

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// A network source goes through temp files: one created before DoCreate fails for the next track is closed and removed.
func TestRemuxToHLSCleansTempsWhenCreateFails(t *testing.T) {
	src := openGOPSource(t, 3, false, true)
	out := t.TempDir()
	var created []string
	ffs := &mkv.FS{Create: func(p string) (mkv.WriteSeekCloser, error) {
		if strings.HasPrefix(filepath.Base(p), ".mkvgo-hls-t") {
			if len(created) == 1 {
				return nil, errors.New("disk full")
			}
			created = append(created, p)
		}
		return os.Create(p)
	}}
	err := RemuxToHLS(context.Background(), src, out, Options{SegmentMs: openGOPMs, FS: ffs, SourceAccess: SourceNetwork})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the create failure", err)
	}
	if len(created) != 1 {
		t.Fatalf("created %d temps before the failure, want 1", len(created))
	}
	if _, serr := os.Stat(created[0]); !errors.Is(serr, fs.ErrNotExist) {
		t.Fatalf("%s left behind (stat: %v)", created[0], serr)
	}
}
