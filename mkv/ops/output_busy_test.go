package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/tmplock"
	"github.com/gravity-zero/mkvgo/mkv"
)

// liveTemp stands for a run writing path's temporary file at this moment: the file exists, locked.
func liveTemp(t *testing.T, tmp string) {
	t.Helper()
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	mustNil(t, err)
	_, err = f.WriteString("half written by a live run")
	mustNil(t, err)
	l, err := tmplock.Take(tmp, f)
	mustNil(t, err)
	t.Cleanup(func() { _ = f.Close(); l.Release() })
}

// TestOutputBusy: a second run aiming at an output removed the temporary file the first was still writing and
// wrote its own under the name, so the first renamed the second's half-written file into place, or the second's
// output vanished. The second run is refused while the first holds its temporary file, and nothing of the first's
// is touched.
func TestOutputBusy(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "netbsd", "openbsd", "dragonfly", "windows":
	default:
		t.Skip("no file locks on this system")
	}
	ctx := context.Background()
	for _, op := range []struct {
		name string
		run  func(src, dst string) (target string, err error)
	}{
		{"Reindex", func(src, dst string) (string, error) { return dst, Reindex(ctx, src, dst) }},
		{"ReindexReplace", func(src, _ string) (string, error) { return src, ReindexReplace(ctx, src) }},
		{"RetimeTracksReplace", func(src, _ string) (string, error) {
			return src, RetimeTracksReplace(ctx, src, map[uint64]int64{1: 40_000_000})
		}},
	} {
		dir := t.TempDir()
		src := filepath.Join(dir, "film.mkv")
		tailTagsMKV(t, src, 2)
		dst := filepath.Join(dir, "out.mkv")
		writeAll(t, dst, []byte("an earlier result"))
		target := dst
		if op.name != "Reindex" {
			target = src
		}
		before, err := os.ReadFile(target)
		mustNil(t, err)
		liveTemp(t, target+".mkvgo.tmp")

		if _, err := op.run(src, dst); !errors.Is(err, mkv.ErrOutputBusy) {
			t.Errorf("%s while another run writes the output: %v, want ErrOutputBusy", op.name, err)
		}
		if got, _ := os.ReadFile(target + ".mkvgo.tmp"); string(got) != "half written by a live run" {
			t.Errorf("%s: the live run's temporary file was touched: %q", op.name, got)
		}
		if got, _ := os.ReadFile(target); string(got) != string(before) {
			t.Errorf("%s: the output was replaced", op.name)
		}
	}
}

// TestReplaceLeftoverReplaced: the replace operations refused a temporary file a killed run left, where every
// other output replaces it; nothing holds it, so they replace it too.
func TestReplaceLeftoverReplaced(t *testing.T) {
	ctx := context.Background()
	for _, op := range []struct {
		name string
		run  func(path string) error
	}{
		{"ReindexReplace", func(p string) error { return ReindexReplace(ctx, p) }},
		{"RetimeTracksReplace", func(p string) error { return RetimeTracksReplace(ctx, p, map[uint64]int64{1: 40_000_000}) }},
	} {
		path := filepath.Join(t.TempDir(), "film.mkv")
		tailTagsMKV(t, path, 2)
		writeAll(t, path+".mkvgo.tmp", []byte("left by a killed run"))
		if err := op.run(path); err != nil {
			t.Errorf("%s over a leftover: %v", op.name, err)
		}
		if _, err := os.Stat(path + ".mkvgo.tmp"); !os.IsNotExist(err) {
			t.Errorf("%s: the leftover is still there: %v", op.name, err)
		}
	}
}
